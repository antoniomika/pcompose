// Package hook implements the necessary handle for git hooks used by pcompose
package hook

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/antoniomika/pcompose/utils"
	"github.com/spf13/viper"
)

// Start initializes the git hook command.
func Start() {
	hookType := strings.TrimPrefix(os.Args[0], utils.HooksDirName)

	repoDir, err := os.Getwd()
	if err != nil {
		log.Println("Error getting working directory:", err)
	}

	data, err := ioutil.ReadAll(os.Stdin)
	if err != nil {
		log.Println("Error reading from stdin:", err)
	}

	commandArgs := strings.Fields(strings.TrimSpace(string(data)))

	var oldRev string
	var newRev string
	var refName string

	if len(commandArgs) > len(os.Args) {
		oldRev = commandArgs[0]
		newRev = commandArgs[1]
		refName = commandArgs[2]
	} else {
		oldRev = os.Args[2]
		newRev = os.Args[3]
		refName = os.Args[1]
	}

	switch hookType {
	case "pre-receive":
		handlePreReceive(hookType, repoDir, oldRev, newRev, refName)
	case "update":
		handleUpdate(hookType, repoDir, oldRev, newRev, refName)
	case "post-receive":
		handlePostReceive(hookType, repoDir, oldRev, newRev, refName)
	default:
		log.Println("Undefined hook type:", hookType)
		return
	}
}

func handlePreReceive(hookType, repoDir, oldRev, newRev, refName string) {
}

func handleUpdate(hookType, repoDir, oldRev, newRev, refName string) {
}

func handlePostReceive(hookType, repoDir, oldRev, newRev, refName string) {
	deploymentDir := path.Join(repoDir, path.Base(repoDir))

	if _, err := os.Stat(deploymentDir); os.IsNotExist(err) {
		cloneCmd := exec.Command("git", "clone", repoDir, deploymentDir)
		err := cloneCmd.Run()
		if err != nil {
			log.Println("Error cloning git repository:", err)
			os.Exit(1)
		}
	} else {
		fetchCmd := exec.Command("git", "fetch")
		fetchCmd.Dir = deploymentDir
		fetchCmd.Env = append(fetchCmd.Env, "GIT_DIR=.git")
		err := fetchCmd.Run()
		if err != nil {
			log.Println("Error fetching git repository:", err)
			os.Exit(1)
		}

		mainBranchCmd := exec.Command("sh", "-c", "git symbolic-ref refs/remotes/origin/HEAD | sed 's@^refs/remotes/origin/@@'")
		mainBranchCmd.Dir = deploymentDir
		mainBranchCmd.Env = append(mainBranchCmd.Env, "GIT_DIR=.git")

		mainBranch, err := mainBranchCmd.Output()
		if err != nil {
			log.Println("Error getting main branch:", err)
			os.Exit(1)
		}

		log.Println("Main branch:", mainBranch, err)
		log.Println("git", "reset", fmt.Sprintf("origin/%s", mainBranch), "--hard")

		resetCmd := exec.Command("git", "reset", fmt.Sprintf("origin/%s", mainBranch), "--hard")
		resetCmd.Dir = deploymentDir
		resetCmd.Env = append(resetCmd.Env, "GIT_DIR=.git")
		err = resetCmd.Run()
		if err != nil {
			log.Println("Error resetting git repository:", err)
			os.Exit(1)
		}
	}

	executable, err := os.Executable()
	if err != nil {
		log.Println("Error getting executable path:", err)
	}

	executablePath, err := filepath.EvalSymlinks(executable)
	if err != nil {
		log.Println("Unable to evaluate symlink:", err)
	}

	appDir := path.Dir(executablePath)

	dataDir := viper.GetString("data-directory")
	if !path.IsAbs(dataDir) {
		dataDir = path.Join(appDir, dataDir)
	}

	pathSlice := strings.Split(strings.TrimPrefix(repoDir, dataDir+string(os.PathSeparator)), string(os.PathSeparator))
	composeProject := strings.Join(pathSlice, "_")

	networkName := fmt.Sprintf("%s_default", composeProject)

	networkCreate := exec.Command("docker", "network", "create", networkName)
	networkCreate.Dir = deploymentDir
	_ = networkCreate.Run()

	networkConnect := exec.Command("docker", "network", "connect", networkName, viper.GetString("frontend-container-name"))
	networkConnect.Dir = deploymentDir
	_ = networkConnect.Run()

	cmd := exec.Command("docker-compose", "-p", composeProject, "up", "-d", "--build")

	cmd.Dir = deploymentDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err = cmd.Run()
	if err != nil {
		log.Println("Error running docker-compose up:", err)
		os.Exit(1)
	}
}
