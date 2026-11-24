// Package main represents the main entrypoint of the pcompose application.
package main

import (
	"log/slog"
	"os"

	"github.com/antoniomika/pcompose/cmd"
	"github.com/antoniomika/pcompose/hook"
)

// main runs pcompose, or the git hook it was invoked as.
func main() {
	if name := hook.Invoked(); name != "" {
		os.Exit(hook.Run(name))
	}

	if err := cmd.Execute(); err != nil {
		slog.Error("pcompose failed", "err", err)
		os.Exit(1)
	}
}
