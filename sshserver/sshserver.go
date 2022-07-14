// Package sshserver implements an ssh server used by pcompose
package sshserver

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	pUtils "github.com/antoniomika/pcompose/utils"
	"github.com/antoniomika/sish/utils"
	"github.com/creack/pty"
	"github.com/spf13/viper"
	"golang.org/x/crypto/ssh"
)

// Start intiializes the ssh server for pcompose.
func Start() {
	utils.WatchKeys()

	if viper.GetBool("debug") {
		go func() {
			for {
				log.Println("=======Start=========")
				log.Println("===Goroutines=====")
				log.Println(runtime.NumGoroutine())
				log.Print("========End==========\n\n")

				time.Sleep(2 * time.Second)
			}
		}()
	}

	log.Println("Starting SSH service on address:", viper.GetString("ssh-address"))

	sshConfig := utils.GetSSHConfig()

	listener, err := net.Listen("tcp", viper.GetString("ssh-address"))
	if err != nil {
		log.Fatal(err)
	}

	defer func() {
		listener.Close()
	}()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		for range c {
			os.Exit(0)
		}
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Println(err)
			continue
		}

		if err != nil {
			conn.Close()
			continue
		}

		log.Println("Accepted SSH connection for:", conn.RemoteAddr())

		go func() {
			sshConn, chans, reqs, err := ssh.NewServerConn(conn, sshConfig)
			if err != nil {
				conn.Close()
				log.Println("Error upgrading ssh connection:", err)
				return
			}

			internalSSHConn := &pUtils.SSHConnHolder{
				MainConn: sshConn,
			}

			go handleRequests(internalSSHConn, reqs, nil)
			go handleChannels(internalSSHConn, chans)

			err = sshConn.Wait()
			if err != nil {
				log.Println("Error waiting for ssh connection:", err)
			}
		}()
	}
}

func handleRequests(sshConn *pUtils.SSHConnHolder, reqs <-chan *ssh.Request, channel ssh.Channel) {
	for req := range reqs {
		if viper.GetBool("debug") {
			log.Println("Main Request Info", req.Type, req.WantReply, string(req.Payload))
		}
		go handleRequest(sshConn, req, channel)
	}
}

func handleRequest(sshConn *pUtils.SSHConnHolder, newRequest *ssh.Request, channel ssh.Channel) {
	exitStatus := func() {
		_, err := channel.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
		if err != nil {
			log.Println("Error sending request to channel:", err)
		}

		err = channel.Close()
		if err != nil && viper.GetBool("debug") {
			log.Println("Error closing channel:", err)
		}
	}

	switch req := newRequest.Type; req {
	case "pty-req":
		err := newRequest.Reply(true, nil)
		if err != nil {
			log.Println("Error sending request:", err)
			return
		}

		sshConn.Mu.Lock()
		termLen := newRequest.Payload[3]
		w, h := pUtils.ParseDims(newRequest.Payload[termLen+4:])
		sshConn.W = w
		sshConn.H = h
		sshConn.Mu.Unlock()
	case "shell":
		defer exitStatus()

		err := newRequest.Reply(true, nil)
		if err != nil {
			log.Println("Error sending request:", err)
			return
		}

		var cmd *exec.Cmd

		containerName := sshConn.MainConn.User()

		if strings.HasPrefix(containerName, "c-") {
			containerName = strings.TrimPrefix(containerName, "c-")
			cmd = exec.Command("docker", "exec", "-it", containerName, "/bin/sh")
		} else if strings.HasPrefix(containerName, "l-") {
			containerName = strings.TrimPrefix(containerName, "l-")
			cmd = exec.Command("docker", "logs", "-f", containerName)
		} else if strings.HasPrefix(containerName, "a-") {
			containerName = strings.TrimPrefix(containerName, "a-")
			cmd = exec.Command("docker", "attach", containerName)
		} else {
			_, dirName := filepath.Split(containerName)
			workDir := path.Join(viper.GetString("data-directory"), containerName, dirName)

			if _, err := os.Stat(workDir); err == nil {
				cmd = exec.Command("docker", []string{
					"exec",
					"-it",
					"-w",
					workDir,
					"-e",
					fmt.Sprintf("COMPOSE_PROJECT_NAME=%s", strings.ReplaceAll(containerName, string(os.PathSeparator), "_")),
					viper.GetString("pcompose-container-name"),
					"/bin/zsh",
				}...)
			} else {
				realCmd := "/bin/sh"
				if containerName == viper.GetString("pcompose-container-name") {
					realCmd = "/bin/zsh"
				}
				cmd = exec.Command("docker", "exec", "-it", containerName, realCmd)
			}
		}

		term, dataHandler, err := pty.Open()
		if err != nil {
			log.Println("Error assigning pty:", err)
		}

		cmd.Stdin = dataHandler
		cmd.Stdout = dataHandler
		cmd.Stderr = dataHandler

		sshConn.Mu.Lock()
		pUtils.SetWinSize(term.Fd(), sshConn.W, sshConn.H)

		sshConn.Term = term
		sshConn.Mu.Unlock()

		err = cmd.Start()
		if err != nil {
			log.Println("error starting command")
		}

		go func() {
			_, err := io.Copy(term, channel)
			if err != nil && viper.GetBool("debug") {
				log.Println("Error copying from channel:", err)
			}
		}()

		go func() {
			_, err = io.Copy(channel, term)
			if err != nil && viper.GetBool("debug") {
				log.Println("Error copying from term:", err)
			}
		}()

		err = cmd.Wait()
		if err != nil {
			log.Println("Error waiting for command:", err)
		}

		err = term.Close()
		if err != nil && viper.GetBool("debug") {
			log.Println("Error closing term:", err)
		}
	case "window-change":
		w, h := pUtils.ParseDims(newRequest.Payload)
		sshConn.Mu.Lock()

		sshConn.W = w
		sshConn.H = h

		pUtils.SetWinSize(sshConn.Term.Fd(), w, h)
		sshConn.Mu.Unlock()
	case "exec":
		defer exitStatus()

		payload := string(bytes.ReplaceAll(newRequest.Payload[4:], []byte{'\''}, []byte{}))

		var runCmd *exec.Cmd
		openStdin := false

		if strings.HasPrefix(payload, pUtils.UploadPackServiceName) || strings.HasPrefix(payload, pUtils.ReceivePackServiceName) {
			runCmd = handleGit(payload)
			openStdin = true
		} else {
			containerName := sshConn.MainConn.User()
			_, dirName := filepath.Split(containerName)
			workDir := path.Join(viper.GetString("data-directory"), containerName, dirName)
			composeProject := strings.ReplaceAll(containerName, string(os.PathSeparator), "_")
			networkName := fmt.Sprintf("%s_default", composeProject)

			if strings.Contains(payload, "down") || strings.Contains(payload, "up") {
				command := "disconnect"
				if strings.Contains(payload, "up") {
					command = "connect"

					networkCreate := exec.Command("docker", "network", "create", networkName)
					networkCreate.Dir = workDir
					_ = networkCreate.Run()
				}

				networkHandle := exec.Command("docker", "network", command, networkName, viper.GetString("frontend-container-name"))
				networkHandle.Dir = workDir
				_ = networkHandle.Run()
			}

			runCmd = exec.Command("docker-compose", strings.Fields(payload)...)
			runCmd.Dir = workDir
			runCmd.Env = append(runCmd.Env, fmt.Sprintf("COMPOSE_PROJECT_NAME=%s", composeProject))
		}

		if runCmd == nil {
			err := newRequest.Reply(false, nil)
			if err != nil {
				log.Println("Error sending request:", err)
				return
			}

			return
		}

		err := newRequest.Reply(true, nil)
		if err != nil {
			log.Println("Error sending request:", err)
			return
		}

		if openStdin {
			runCmd.Stdin = channel
		}

		runCmd.Stderr = channel.Stderr()
		runCmd.Stdout = channel

		err = runCmd.Run()
		if err != nil {
			log.Println("Error executing command:", err)
			return
		}
	default:
		err := newRequest.Reply(false, nil)
		if err != nil {
			log.Println("Error replying to socket request:", err)
		}
	}
}

func handleChannels(sshConn *pUtils.SSHConnHolder, chans <-chan ssh.NewChannel) {
	for newChannel := range chans {
		if viper.GetBool("debug") {
			log.Println("Main Channel Info", newChannel.ChannelType(), string(newChannel.ExtraData()))
		}
		go handleChannel(sshConn, newChannel)
	}
}

func handleChannel(sshConn *pUtils.SSHConnHolder, newChannel ssh.NewChannel) {
	switch channel := newChannel.ChannelType(); channel {
	case "session":
		sshChan, reqs, err := newChannel.Accept()
		if err != nil {
			log.Println("Error rejecting socket channel:", err)
		}

		go handleRequests(sshConn, reqs, sshChan)
	default:
		err := newChannel.Reject(ssh.UnknownChannelType, fmt.Sprintf("unknown channel type: %s", channel))
		if err != nil {
			log.Println("Error rejecting socket channel:", err)
		}
	}
}
