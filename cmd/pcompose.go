// Package cmd implements the pcompose CLI command.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/antoniomika/pcompose/sshserver"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
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
		Use:           "pcompose",
		Short:         "The pcompose command runs the pcompose PaaS",
		Long:          "pcompose deploys git repositories pushed to it over SSH on your docker engine, with docker compose",
		Args:          cobra.NoArgs,
		RunE:          runCommand,
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
)

// init initializes flags used by the root command.
func init() {
	rootCmd.SetVersionTemplate(fmt.Sprintf("Version: %v\nCommit: %v\nDate: %v\n", Version, Commit, Date))

	flags := rootCmd.PersistentFlags()

	flags.StringVarP(&configFile, "config", "c", "config.yml", "Config file")

	flags.StringP("ssh-address", "a", "localhost:2222", "The address to listen for SSH connections on")
	flags.StringP("private-key-passphrase", "p", "", "Passphrase for encrypted SSH host keys in the private keys directory")
	flags.StringP("private-keys-directory", "l", "deploy/keys", "Directory of SSH host keys. pcompose generates ssh_host_ed25519_key in it")
	flags.StringP("authentication-keys-directory", "k", "deploy/pubkeys", "Directory of the public keys that may log in. Changes apply at the next login")
	flags.String("data-directory", "deploy/data", "Directory that holds repositories, checkouts, and pcompose's git hooks. The docker engine must see it at the same path for relative bind mounts to work")
	flags.String("runtime-directory", "", "Directory that holds the git hooks and their socket, which only pcompose's user may use. Defaults to run in the data directory")
	flags.String("ingress-network", "picd-ingress", "The docker network the ingress reaches apps on, created if it's missing. Empty creates none")
	flags.Duration("deploy-timeout", 30*time.Minute, "How long a deploy may take")
	flags.Duration("shutdown-timeout", 5*time.Minute, "How long pushes, commands, and deploys have to finish when pcompose is asked to stop")
	flags.Bool("debug", false, "Log debugging information, which includes commands' arguments")
}

// loadConfig reads settings from flags, the environment, and the config file,
// in that order. The environment's are named like PCOMPOSE_SSH_ADDRESS.
func loadConfig(cmd *cobra.Command) (sshserver.Config, error) {
	if err := viper.BindPFlags(cmd.PersistentFlags()); err != nil {
		return sshserver.Config{}, err
	}

	viper.SetConfigFile(configFile)
	viper.SetEnvPrefix("pcompose")
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err != nil {
		var missing viper.ConfigFileNotFoundError
		if !errors.As(err, &missing) && !errors.Is(err, os.ErrNotExist) {
			return sshserver.Config{}, fmt.Errorf("read %s: %w", configFile, err)
		}
	}

	return sshserver.Config{
		SSHAddress:           viper.GetString("ssh-address"),
		DataDirectory:        viper.GetString("data-directory"),
		RuntimeDirectory:     viper.GetString("runtime-directory"),
		PrivateKeysDirectory: viper.GetString("private-keys-directory"),
		PrivateKeyPassphrase: viper.GetString("private-key-passphrase"),
		KeysDirectory:        viper.GetString("authentication-keys-directory"),
		IngressNetwork:       viper.GetString("ingress-network"),
		DeployTimeout:        viper.GetDuration("deploy-timeout"),
		ShutdownTimeout:      viper.GetDuration("shutdown-timeout"),
		Debug:                viper.GetBool("debug"),
	}, nil
}

// Execute executes the root command.
func Execute() error {
	return rootCmd.Execute()
}

// runCommand serves pcompose until it's asked to stop.
func runCommand(cmd *cobra.Command, _ []string) error {
	cfg, err := loadConfig(cmd)
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	srv, err := sshserver.New(cfg, logger)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A second signal stops pcompose at once.
	context.AfterFunc(ctx, stop)

	return srv.Run(ctx)
}
