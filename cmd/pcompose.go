// Package cmd implements the pcompose CLI command.
package cmd

import (
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"strings"
	"time"

	"github.com/antoniomika/pcompose/hook"
	"github.com/antoniomika/pcompose/sshserver"
	pUtils "github.com/antoniomika/pcompose/utils"
	"github.com/antoniomika/sish/utils"
	"github.com/fsnotify/fsnotify"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"gopkg.in/natefinch/lumberjack.v2"
)

var (
	// Version describes the version of the current build.
	Version = "dev"

	// Commit describes the commit of the current build.
	Commit = "none"

	// Date describes the date of the current build.
	Date = "unknown"

	// configFile holds the location of the config file from CLI flags.
	configFile string

	// rootCmd is the root cobra command.
	rootCmd = &cobra.Command{
		Use:     "pcompose",
		Short:   "The pcompose command initializes and runs the pcompose PaaS",
		Long:    "pcompose is a command line utility that runs a simple PaaS ontop of docker using docker-compose and git",
		Run:     runCommand,
		Version: Version,
	}
)

// init initializes flags used by the root command.
func init() {
	cobra.OnInitialize(initConfig)

	rootCmd.SetVersionTemplate(fmt.Sprintf("Version: %v\nCommit: %v\nDate: %v\n", Version, Commit, Date))

	rootCmd.PersistentFlags().StringVarP(&configFile, "config", "c", "config.yml", "Config file")

	rootCmd.PersistentFlags().StringP("ssh-address", "a", "localhost:2222", "The address to listen for SSH connections")
	rootCmd.PersistentFlags().StringP("banned-ips", "x", "", "A comma separated list of banned ips that are unable to access the service. Applies to SSH connections")
	rootCmd.PersistentFlags().StringP("banned-countries", "o", "", "A comma separated list of banned countries. Applies to SSH connections")
	rootCmd.PersistentFlags().StringP("whitelisted-ips", "w", "", "A comma separated list of whitelisted ips. Applies to SSH connections")
	rootCmd.PersistentFlags().StringP("whitelisted-countries", "y", "", "A comma separated list of whitelisted countries. Applies to SSH connections")
	rootCmd.PersistentFlags().StringP("private-key-passphrase", "p", "S3Cr3tP4$$phrAsE", "Passphrase to use to encrypt the server private key")
	rootCmd.PersistentFlags().StringP("private-keys-directory", "l", "deploy/keys", "The location of other SSH server private keys. sish will add these as valid auth methods for SSH. Note, these need to be unencrypted OR use the private-key-passphrase")
	rootCmd.PersistentFlags().StringP("authentication-password", "u", "", "Password to use for ssh server password authentication")
	rootCmd.PersistentFlags().StringP("authentication-keys-directory", "k", "deploy/pubkeys/", "Directory where public keys for public key authentication are stored.\npcompose will watch this directory and automatically load new keys and remove keys\nfrom the authentication list")
	rootCmd.PersistentFlags().StringP("time-format", "", "2006/01/02 - 15:04:05", "The time format to use for general log messages")
	rootCmd.PersistentFlags().StringP("log-to-file-path", "", "/tmp/pcompose.log", "The file to write log output to")
	rootCmd.PersistentFlags().StringP("data-directory", "", "deploy/data/", "Directory that holds pcompose data")
	rootCmd.PersistentFlags().StringP("frontend-container-name", "", "nginx-proxy", "The name of the frontend container in order to connect it to the default docker-compose network.")
	rootCmd.PersistentFlags().StringP("pcompose-container-name", "", "pcompose", "The name of the pcompose container in order to exec into a context.")

	rootCmd.PersistentFlags().BoolP("cleanup-unbound", "", true, "Cleanup unbound (unforwarded) SSH connections after a set timeout")
	rootCmd.PersistentFlags().BoolP("debug", "", false, "Enable debugging information")
	rootCmd.PersistentFlags().BoolP("geodb", "", false, "Use a geodb to verify country IP address association for IP filtering")
	rootCmd.PersistentFlags().BoolP("authentication", "", false, "Require authentication for the SSH service")
	rootCmd.PersistentFlags().BoolP("log-to-stdout", "", true, "Enable writing log output to stdout")
	rootCmd.PersistentFlags().BoolP("log-to-file", "", false, "Enable writing log output to file, specified by log-to-file-path")
	rootCmd.PersistentFlags().BoolP("log-to-file-compress", "", false, "Enable compressing log output files")

	rootCmd.PersistentFlags().IntP("log-to-file-max-size", "", 500, "The maximum size of outputed log files in megabytes")
	rootCmd.PersistentFlags().IntP("log-to-file-max-backups", "", 3, "The maxium number of rotated logs files to keep")
	rootCmd.PersistentFlags().IntP("log-to-file-max-age", "", 28, "The maxium number of days to store log output in a file")

	rootCmd.PersistentFlags().DurationP("authentication-keys-directory-watch-interval", "", 200*time.Millisecond, "The interval to poll for filesystem changes for SSH keys")
}

// initConfig initializes the configuration and loads needed
// values. It initializes logging and other vars.
func initConfig() {
	writeConfigChanges := true

	if strings.HasPrefix(os.Args[0], pUtils.HooksDirName) {
		writeConfigChanges = false

		workingDir, err := os.Getwd()
		if err != nil {
			log.Println("Error getting working directory:", err)
		} else {
			configFile = path.Join(workingDir, pUtils.HooksDirName, pUtils.HooksConfigFile)
		}
	}

	viper.SetConfigFile(configFile)

	err := viper.BindPFlags(rootCmd.PersistentFlags())
	if err != nil {
		log.Println("Unable to bind pflags:", err)
	}

	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err == nil && writeConfigChanges {
		log.Println("Using config file:", viper.ConfigFileUsed())
	}

	viper.WatchConfig()

	writers := []io.Writer{}

	if viper.GetBool("log-to-stdout") {
		writers = append(writers, os.Stdout)
	}

	if viper.GetBool("log-to-file") {
		writers = append(writers, &lumberjack.Logger{
			Filename:   viper.GetString("log-to-file-path"),
			MaxSize:    viper.GetInt("log-to-file-max-size"),
			MaxBackups: viper.GetInt("log-to-file-max-backups"),
			MaxAge:     viper.GetInt("log-to-file-max-age"),
			Compress:   viper.GetBool("log-to-file-compress"),
		})
	}

	multiWriter := io.MultiWriter(writers...)

	writeConfigFile := path.Join(viper.GetString("data-directory"), pUtils.HooksConfigFile)

	viper.OnConfigChange(func(e fsnotify.Event) {
		log.Println("Reloaded configuration file.")

		log.SetFlags(0)
		log.SetOutput(utils.LogWriter{
			TimeFmt:     viper.GetString("time-format"),
			MultiWriter: multiWriter,
		})

		if viper.GetBool("debug") {
			logrus.SetLevel(logrus.DebugLevel)
		}

		if writeConfigChanges {
			err := viper.WriteConfigAs(writeConfigFile)
			if err != nil {
				log.Println("Error writing config for hooks")
			}
		}
	})

	log.SetFlags(0)
	log.SetOutput(utils.LogWriter{
		TimeFmt:     viper.GetString("time-format"),
		MultiWriter: multiWriter,
	})

	if viper.GetBool("debug") {
		logrus.SetLevel(logrus.DebugLevel)
	}

	logrus.SetOutput(multiWriter)

	utils.Setup(multiWriter)

	if writeConfigChanges {
		err := viper.WriteConfigAs(writeConfigFile)
		if err != nil {
			log.Println("Error writing config for hooks")
		}
	}
}

// Execute executes the root command.
func Execute() error {
	return rootCmd.Execute()
}

// runCommand is used to start the root muxer.
func runCommand(cmd *cobra.Command, args []string) {
	if strings.HasPrefix(os.Args[0], pUtils.HooksDirName) {
		hook.Start()
		os.Exit(0)
	}

	sshserver.Start()
}
