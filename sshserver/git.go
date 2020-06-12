package sshserver

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path"
	"strings"

	pUtils "github.com/antoniomika/pcompose/utils"
	"github.com/spf13/viper"
)

func handleGit(payload string) *exec.Cmd {
	var runCmd *exec.Cmd
	commandData := strings.Fields(payload)

	repoDir := strings.TrimSuffix(path.Join(viper.GetString("data-directory"), commandData[1]), ".git")

	if _, err := os.Stat(repoDir); os.IsNotExist(err) {
		err := os.MkdirAll(repoDir, os.FileMode(0755))
		if err != nil {
			log.Println("Error creating directory:", err)
		}

		initCmd := exec.Command("git", "init", "--bare")
		initCmd.Env = append(initCmd.Env, fmt.Sprintf("GIT_DIR=%s", repoDir))

		err = initCmd.Run()
		if err != nil {
			log.Println("Error creating repository:", err)
		}
	}

	hooksDir := path.Join(repoDir, pUtils.HooksDirName)

	for _, hook := range []string{"pre-receive", "update", "post-receive"} {
		hookName := path.Join(hooksDir, hook)

		executable, err := os.Executable()
		if err != nil {
			log.Println("Error getting executable:", err)
			return runCmd
		}

		err = os.Symlink(executable, hookName)
		if err != nil {
			log.Println("Error symlinking file:", err)
		}

		err = os.Chmod(hookName, os.ModePerm)
		if err != nil {
			log.Println("Error chmoding file:", err)
			return runCmd
		}
	}

	err := os.Symlink(path.Join(viper.GetString("data-directory"), pUtils.HooksConfigFile), path.Join(hooksDir, pUtils.HooksConfigFile))
	if err != nil {
		log.Println("Error symlinking file:", err)
	}

	if strings.HasPrefix(payload, pUtils.UploadPackServiceName) {
		runCmd = exec.Command(pUtils.UploadPackServiceName, repoDir)
	} else if strings.HasPrefix(payload, pUtils.ReceivePackServiceName) {
		runCmd = exec.Command(pUtils.ReceivePackServiceName, repoDir)
	}

	return runCmd
}
