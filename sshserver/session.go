package sshserver

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/antoniomika/pcompose/compose"
	"github.com/antoniomika/pcompose/run"
	"github.com/picosh/pico/pkg/pssh"
)

const (
	uploadPack  = "git-upload-pack"
	receivePack = "git-receive-pack"
)

// usageError is an error to show the user as it is.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error {
	return &usageError{fmt.Sprintf(format, args...)}
}

// activeSessions holds the SSH sessions pcompose is serving, so it can drain
// them.
type activeSessions struct {
	mu       sync.Mutex
	draining bool
	// sessions maps each one to whether it's interactive.
	sessions map[*pssh.SSHServerConnSession]bool
}

// add holds s, unless pcompose is draining.
func (a *activeSessions) add(s *pssh.SSHServerConnSession, interactive bool) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.draining {
		return false
	}

	if a.sessions == nil {
		a.sessions = map[*pssh.SSHServerConnSession]bool{}
	}
	a.sessions[s] = interactive

	return true
}

func (a *activeSessions) remove(s *pssh.SSHServerConnSession) {
	a.mu.Lock()
	defer a.mu.Unlock()

	delete(a.sessions, s)
}

// drain turns new sessions away, ends the interactive ones, and returns them.
func (a *activeSessions) drain() (interactive []*pssh.SSHServerConnSession) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.draining = true
	for s, ok := range a.sessions {
		if ok {
			interactive = append(interactive, s)
		}
	}

	return interactive
}

// busy returns how many sessions that aren't interactive remain.
func (a *activeSessions) busy() int {
	a.mu.Lock()
	defer a.mu.Unlock()

	n := 0
	for _, interactive := range a.sessions {
		if !interactive {
			n++
		}
	}

	return n
}

// drain lets the work in flight finish: new sessions are turned away, shells
// and log streams end, and pcompose waits for pushes, commands, and deploys.
func (srv *Server) drain() {
	interactive := srv.active.drain()
	for _, s := range interactive {
		_, _ = fmt.Fprint(run.Stderr(s), "\npcompose is stopping.\n")
		_ = s.Exit(1)
		_ = s.Close()
	}

	srv.logger.Info("Draining", "closedSessions", len(interactive), "timeout", srv.cfg.ShutdownTimeout)

	deadline := time.After(srv.cfg.ShutdownTimeout)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for srv.active.busy() > 0 || srv.work.Load() > 0 {
		select {
		case <-deadline:
			srv.logger.Warn("Stopped draining with work left", "busySessions", srv.active.busy(), "work", srv.work.Load())
			return
		case <-ticker.C:
		}
	}

	srv.logger.Info("Drained")
}

func (srv *Server) middleware(pssh.SSHServerHandler) pssh.SSHServerHandler {
	return func(s *pssh.SSHServerConnSession) error {
		argv := s.Command()
		cmd, args := findCommand(argv)
		_, _, pty := s.Pty()

		interactive := len(argv) == 0 || pty && cmd == nil || cmd != nil && cmd.interactive(args, pty)

		err := errors.New("pcompose is stopping, reconnect in a moment")
		if srv.active.add(s, interactive) {
			err = srv.handle(s, cmd, args)
			srv.active.remove(s)
		}

		if err == nil {
			return nil
		}

		var usage *usageError
		var flag *compose.FlagError
		switch {
		case s.Err() != nil:
			// The client left.
		case isExit(err):
			// The command said why itself.
		case errors.As(err, &usage), errors.As(err, &flag):
			_, _ = fmt.Fprintf(run.Stderr(s), "Error: %v\n", err)
		default:
			srv.logger.Error("Session failed", "target", s.User(), "err", err)
			_, _ = fmt.Fprintf(run.Stderr(s), "Error: %v\n", err)
		}

		_ = s.Exit(run.ExitCode(err))
		_ = s.Close()

		return err
	}
}

func (srv *Server) handle(s *pssh.SSHServerConnSession, cmd *command, args []string) error {
	argv := s.Command()

	switch {
	case cmd != nil:
		srv.logger.Info("Running command", "command", cmd.name, "target", s.User())
		return srv.runCommand(s, cmd, args)
	case len(argv) == 2 && slices.Contains([]string{uploadPack, receivePack}, argv[0]):
		srv.logger.Info("Serving git", "service", argv[0], "repo", argv[1])
		return srv.serveGit(s, argv[0], argv[1])
	default:
		srv.logger.Info("Starting session", "target", s.User(), "command", len(argv) > 0)
		return srv.runTarget(s, argv)
	}
}

// containerTarget splits a c-, l-, or a- prefix off a target naming a
// container.
func containerTarget(target string) (prefix, name string) {
	for _, prefix := range []string{"c-", "l-", "a-"} {
		if name, ok := strings.CutPrefix(target, prefix); ok {
			return prefix, name
		}
	}

	return "", target
}

// runTarget runs argv, or a shell, in what the SSH user names: a project, a
// container, or else the projects directory.
func (srv *Server) runTarget(s *pssh.SSHServerConnSession, argv []string) error {
	ctx := s.Context()
	_, _, pty := s.Pty()

	name := ""
	if srv.exists(s.User()) {
		name = s.User()
	}

	if name == "" {
		// Projects come first, so one named c-api can be reached.
		prefix, container := containerTarget(s.User())

		if compose.HasContainer(ctx, container) {
			switch {
			case prefix == "l-":
				return run.Exec(s, run.Opts{Cmd: []string{"docker", "logs", "-f", container}, TTY: true})
			case prefix == "a-":
				cmd := []string{"docker", "attach", "--sig-proxy=false"}
				return run.Exec(s, run.Opts{Cmd: append(cmd, container), Stdin: true, TTY: true})
			case prefix == "c-" || len(argv) > 0:
				return execContainer(s, container, argv)
			}
		} else if prefix != "" {
			return usagef("no container named %s", container)
		}
	}

	if len(argv) == 0 && !pty {
		return usagef("a shell needs a terminal: connect without -T, or name a command. Run 'help' to list them")
	}

	opts := run.Opts{Cmd: argv, Dir: srv.projectsDir(), Stdin: true, TTY: true}

	if name != "" {
		if err := srv.ensureWorktree(ctx, name); err != nil {
			return err
		}

		opts.Dir = srv.WorktreePath(name)
		opts.Env = compose.Env(name, opts.Dir)
	}

	if len(argv) == 0 {
		opts.Cmd = []string{"/bin/sh", "-c", shellScript}
	}

	return run.Exec(s, opts)
}

func execContainer(s run.Session, container string, argv []string) error {
	cmd := []string{"docker", "exec", "-i"}
	if _, _, ok := s.Pty(); ok {
		cmd = append(cmd, "-t")
	}

	if len(argv) == 0 {
		argv = []string{"/bin/sh"}
	}

	return run.Exec(s, run.Opts{Cmd: append(append(cmd, container), argv...), Stdin: true, TTY: true})
}

// shellScript runs a shell. TERM is the SSH client's, which this machine may
// not know.
const shellScript = `infocmp "$TERM" >/dev/null 2>&1 || export TERM=xterm-256color
command -v zsh >/dev/null && exec zsh
exec "${SHELL:-/bin/sh}"`
