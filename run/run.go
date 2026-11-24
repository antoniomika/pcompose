// Package run runs commands on this machine for pcompose's sessions.
package run

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/picosh/pico/pkg/pssh"
)

// killDelay is how long a command has to end once its session does, before
// it's killed.
const killDelay = 5 * time.Second

// Session is the client a command runs for: an SSH session, or a Detached one
// for deploys pcompose starts itself. *pssh.SSHServerConnSession is one.
type Session interface {
	io.ReadWriter
	Stderr() io.ReadWriter
	Context() context.Context
	Pty() (*pssh.Pty, <-chan pssh.Window, bool)
}

// Opts configures a command.
type Opts struct {
	Dir string
	Cmd []string
	// Env is added to pcompose's own environment.
	Env []string
	// Stdin forwards the session's input to the command.
	Stdin bool
	// TTY gives the command a terminal, if the session has one.
	TTY bool
}

func command(o Opts) *exec.Cmd {
	cmd := exec.Command(o.Cmd[0], o.Cmd[1:]...)
	cmd.Dir = o.Dir
	cmd.Env = os.Environ()
	// Go sets it only for commands with no Env.
	if o.Dir != "" {
		cmd.Env = append(cmd.Env, "PWD="+o.Dir)
	}
	cmd.Env = append(cmd.Env, o.Env...)
	return cmd
}

// wait waits for a started command. When ctx ends first, it ends the command's
// process group with sig, and kills it if that wasn't enough. What's left in
// the background counts, since it may hold the command's output open.
func wait(ctx context.Context, cmd *exec.Cmd, sig syscall.Signal) error {
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	select {
	case err := <-waited:
		return err
	case <-ctx.Done():
	}

	for _, sig := range []syscall.Signal{sig, syscall.SIGKILL} {
		_ = syscall.Kill(-cmd.Process.Pid, sig)

		select {
		case <-waited:
			return ctx.Err()
		case <-time.After(killDelay):
		}
	}

	return ctx.Err()
}

// Exec runs a command, streaming it over the session. The command ends if the
// session does.
func Exec(s Session, o Opts) error {
	ctx := s.Context()
	if err := ctx.Err(); err != nil {
		return err
	}

	if o.TTY {
		if p, winch, ok := s.Pty(); ok {
			return execTTY(s, o, p, winch)
		}
	}

	cmd := command(o)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = s
	cmd.Stderr = Stderr(s)

	// Wait would wait for the client to close a Stdin set directly.
	var stdin io.WriteCloser
	if o.Stdin {
		var err error
		if stdin, err = cmd.StdinPipe(); err != nil {
			return err
		}
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	if stdin != nil {
		go func() {
			_, _ = io.Copy(stdin, s)
			_ = stdin.Close()
		}()
	}

	return wait(ctx, cmd, syscall.SIGTERM)
}

func execTTY(s Session, o Opts, p *pssh.Pty, winch <-chan pssh.Window) error {
	if p.Term != "" {
		o.Env = append(o.Env, "TERM="+p.Term)
	}

	cmd := command(o)

	tty, err := pty.StartWithSize(cmd, winsize(p.Window))
	if err != nil {
		return err
	}
	defer func() { _ = tty.Close() }()

	done, resized := make(chan struct{}), make(chan struct{})
	defer func() {
		close(done)
		<-resized
	}()

	go func() {
		defer close(resized)

		for {
			select {
			case <-done:
				return
			case win := <-winch:
				_ = pty.Setsize(tty, winsize(win))
			}
		}
	}()

	go func() { _, _ = io.Copy(tty, s) }()

	copied := make(chan struct{})
	go func() {
		_, _ = io.Copy(terminalOutput(s), tty)
		close(copied)
	}()

	err = wait(s.Context(), cmd, syscall.SIGHUP)

	// Something left in the background may keep the terminal open.
	select {
	case <-copied:
	case <-time.After(time.Second):
	}

	return err
}

func winsize(w pssh.Window) *pty.Winsize {
	return &pty.Winsize{Cols: uint16(w.Width), Rows: uint16(w.Height)}
}

// terminalOutput is where to write what a terminal already produced. pssh
// adds \r before each \n when the client has a PTY, which would move a
// full-screen program's cursor.
func terminalOutput(s Session) io.Writer {
	if s, ok := s.(*pssh.SSHServerConnSession); ok {
		return s.Channel
	}

	return s
}

// Stderr returns the session's stderr, ending lines as its terminal needs.
// pssh does so for stdout.
func Stderr(s Session) io.Writer {
	if _, _, ok := s.Pty(); ok {
		return &crlf{s.Stderr()}
	}

	return s.Stderr()
}

type crlf struct{ w io.Writer }

func (c *crlf) Write(p []byte) (int, error) {
	if _, err := c.w.Write(bytes.ReplaceAll(p, []byte("\n"), []byte("\r\n"))); err != nil {
		return 0, err
	}

	return len(p), nil
}

// Output runs a command and returns its standard output, trimmed. If the
// command fails, the error includes its standard error. It isn't an
// *exec.ExitError, since the command hasn't shown the user why it failed.
func Output(ctx context.Context, o Opts) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	cmd := command(o)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	if err := cmd.Start(); err != nil {
		return "", err
	}

	if err := wait(ctx, cmd, syscall.SIGTERM); err != nil {
		// The command may still be writing.
		if ctx.Err() != nil {
			return "", fmt.Errorf("%s: %w", o.Cmd[0], ctx.Err())
		}

		return "", fmt.Errorf("%s: %v: %s", o.Cmd[0], err, strings.TrimSpace(stderr.String()))
	}

	return strings.TrimSpace(stdout.String()), nil
}

// ExitCode is the exit code of a command that failed with err, or 1.
func ExitCode(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() > 0 {
		return exit.ExitCode()
	}

	return 1
}
