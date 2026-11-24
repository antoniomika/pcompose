// Package hook connects the git hooks a push runs to the pcompose that serves
// the push. Each hook is a link to pcompose's own executable.
package hook

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
)

const (
	// SocketEnv and IDEnv are set for a push's git-receive-pack: where
	// pcompose listens for its hooks, and which push they're for.
	SocketEnv = "PCOMPOSE_HOOK_SOCKET"
	IDEnv     = "PCOMPOSE_HOOK_ID"

	// FailedTrailer is set on the response to a hook that failed.
	FailedTrailer = "Pcompose-Failed"
)

// Names are the hooks pcompose runs.
var Names = []string{"pre-receive", "post-receive"}

// Invoked returns the hook this process was run as, or "".
func Invoked() string {
	name := filepath.Base(os.Args[0])
	if slices.Contains(Names, name) && os.Getenv(SocketEnv) != "" {
		return name
	}

	return ""
}

// Install links the hooks into dir to this executable, unless they already are.
func Install(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	executable, err := os.Executable()
	if err != nil {
		return err
	}

	// Git silently skips hooks it can't run.
	if _, err := os.Stat(executable); err != nil {
		return err
	}

	for _, name := range Names {
		link := filepath.Join(dir, name)
		if current, err := os.Readlink(link); err == nil && current == executable {
			continue
		}

		// Renamed into place, so a push at the same time always finds one.
		tmp, err := os.MkdirTemp(dir, ".new-")
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(tmp) }()

		if err := os.Symlink(executable, filepath.Join(tmp, name)); err != nil {
			return err
		}

		if err := os.Rename(filepath.Join(tmp, name), link); err != nil {
			return err
		}
	}

	return nil
}

// Run passes the hook's input to pcompose and prints what it answers. It
// returns the hook's exit code.
func Run(name string) int {
	socket, id := os.Getenv(SocketEnv), os.Getenv(IDEnv)

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}

	// Reading from a pipe leaves its length unknown, which is fine: the
	// request is sent chunked.
	res, err := client.Post("http://pcompose/"+id+"/"+name, "text/plain", io.NopCloser(os.Stdin))
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "pcompose couldn't be reached:", err)
		return 1
	}
	defer func() { _ = res.Body.Close() }()

	if _, err := io.Copy(os.Stdout, res.Body); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "pcompose stopped answering:", err)
		return 1
	}

	if res.StatusCode != http.StatusOK || res.Trailer.Get(FailedTrailer) != "" {
		return 1
	}

	return 0
}
