// Package sshserver serves pcompose over SSH.
package sshserver

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/antoniomika/pcompose/compose"
	"github.com/antoniomika/pcompose/hook"
	"github.com/antoniomika/pcompose/run"
	"github.com/picosh/pico/pkg/pssh"
	"golang.org/x/crypto/ssh"
)

// Config configures pcompose.
type Config struct {
	SSHAddress string

	// DataDirectory holds repositories and checkouts. The docker engine must
	// see the checkouts at the same path, since it resolves their relative
	// bind mounts.
	DataDirectory string
	// RuntimeDirectory holds the git hooks and their socket, whose path's
	// length is limited. Empty uses the data directory.
	RuntimeDirectory     string
	PrivateKeysDirectory string
	PrivateKeyPassphrase string
	// KeysDirectory holds the public keys that may log in.
	KeysDirectory string

	// IngressNetwork is created if it's missing. Empty creates none.
	IngressNetwork  string
	DeployTimeout   time.Duration
	ShutdownTimeout time.Duration
	Debug           bool
}

// Server is pcompose.
type Server struct {
	cfg    Config
	logger *slog.Logger

	// creating is held while a project is created, since its name is checked
	// against the others.
	creating sync.Mutex
	// deploys holds a lock per project, so one deploy runs at a time.
	deploys run.Locks
	// pushes holds the pushes being received, by id, for their hooks, which
	// answer on hookSocket.
	pushes     sync.Map
	hookSocket string

	active activeSessions
	// work counts the hooks and deploys running, which can outlive their
	// clients.
	work atomic.Int64
}

// New returns a Server with its directories ready.
func New(cfg Config, logger *slog.Logger) (*Server, error) {
	for _, dir := range []*string{&cfg.DataDirectory, &cfg.RuntimeDirectory} {
		if *dir == "" {
			continue
		}

		abs, err := filepath.Abs(*dir)
		if err != nil {
			return nil, err
		}
		*dir = abs
	}

	if cfg.RuntimeDirectory != "" {
		if err := ensureRuntimeDirectory(cfg.RuntimeDirectory); err != nil {
			return nil, err
		}
	}

	// Checkouts hold secrets, in their .env.
	for _, dir := range []string{"repos", "projects", "run"} {
		if err := os.MkdirAll(filepath.Join(cfg.DataDirectory, dir), 0o700); err != nil {
			return nil, err
		}
	}

	srv := &Server{cfg: cfg, logger: logger}

	if err := hook.Install(srv.hooksDir()); err != nil {
		return nil, fmt.Errorf("install git hooks: %w", err)
	}

	return srv, nil
}

// ensureRuntimeDirectory makes sure no one else can reach the socket in dir,
// which may be in a shared directory.
func ensureRuntimeDirectory(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}

	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm() != 0o700 || !ok || int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s must be a directory only its owner, the user pcompose runs as, can use", dir)
	}

	return nil
}

func (srv *Server) runtimeDir() string {
	if srv.cfg.RuntimeDirectory != "" {
		return srv.cfg.RuntimeDirectory
	}

	return filepath.Join(srv.cfg.DataDirectory, "run")
}

func (srv *Server) hooksDir() string {
	return filepath.Join(srv.runtimeDir(), "hooks")
}

func (srv *Server) projectsDir() string {
	return filepath.Join(srv.cfg.DataDirectory, "projects")
}

// RepoPath is the project's bare repository.
func (srv *Server) RepoPath(name string) string {
	return filepath.Join(srv.cfg.DataDirectory, "repos", filepath.FromSlash(name)+".git")
}

// WorktreePath is the project's deployed checkout.
func (srv *Server) WorktreePath(name string) string {
	return filepath.Join(srv.projectsDir(), filepath.FromSlash(name))
}

// Run serves until ctx ends, then lets the work in flight finish.
func (srv *Server) Run(ctx context.Context) error {
	// Cancelled once pcompose has drained, which ends what's left.
	serving, stop := context.WithCancel(context.WithoutCancel(ctx))
	defer stop()

	if _, err := os.Stat(filepath.Join(srv.cfg.DataDirectory, "hooks.yml")); err == nil {
		srv.logger.Warn("The data directory was made by pcompose 0.3 or earlier, whose projects need to be moved. See Upgrading in the README")
	}

	if srv.cfg.IngressNetwork != "" {
		if err := compose.EnsureNetwork(ctx, srv.cfg.IngressNetwork); err != nil {
			return err
		}
	}

	if err := srv.serveHooks(serving); err != nil {
		return err
	}

	server, err := srv.newSSHServer(serving)
	if err != nil {
		return err
	}

	if len(srv.authorizedKeys()) == 0 {
		srv.logger.Warn("No public keys may log in. Add one to the keys directory", "directory", srv.cfg.KeysDirectory)
	}

	srv.logger.Info("Starting SSH server", "address", srv.cfg.SSHAddress)

	served := make(chan error, 1)
	go func() { served <- server.ListenAndServe() }()

	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}

	srv.drain()

	return nil
}

func (srv *Server) newSSHServer(ctx context.Context) (*pssh.SSHServer, error) {
	host, port, err := net.SplitHostPort(srv.cfg.SSHAddress)
	if err != nil {
		return nil, fmt.Errorf("invalid SSH address %q: %w", srv.cfg.SSHAddress, err)
	}

	// pssh logs requests' payloads, which hold commands' arguments.
	level := slog.LevelWarn
	if srv.cfg.Debug {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	server, err := pssh.NewSSHServerWithConfig(
		ctx, logger, "pcompose", host, port, "", "",
		srv.authenticate,
		[]pssh.SSHServerMiddleware{srv.middleware},
		nil, nil,
	)
	if err != nil {
		return nil, fmt.Errorf("create SSH server: %w", err)
	}

	// pssh joins the host and port without brackets for IPv6.
	server.Config.ListenAddr = srv.cfg.SSHAddress

	if err := srv.loadHostKeys(server); err != nil {
		return nil, fmt.Errorf("load host keys: %w", err)
	}

	return server, nil
}

// authenticate accepts the keys in the keys directory, whatever user they
// connect as: they all belong to pcompose's one user.
func (srv *Server) authenticate(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	for _, authorized := range srv.authorizedKeys() {
		if pssh.KeysEqual(authorized, key) {
			srv.logger.Info("Accepted public key", "remoteAddr", conn.RemoteAddr(), "fingerprint", ssh.FingerprintSHA256(key))
			return &ssh.Permissions{Extensions: map[string]string{"fingerprint": ssh.FingerprintSHA256(key)}}, nil
		}
	}

	return nil, errors.New("public key not accepted")
}

// keyFiles reads the files in dir, except hidden ones such as .gitkeep and
// the links Kubernetes mounts Secrets with.
func keyFiles(dir string, fn func(name string, data []byte)) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == dir {
				return err
			}

			slog.Error("Skipping an unreadable key file", "file", path, "err", err)
			return nil
		}

		if path != dir && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			return nil
		}

		// One bad file mustn't keep the others from being read.
		data, err := os.ReadFile(path)
		if err != nil {
			slog.Error("Skipping an unreadable key file", "file", path, "err", err)
			return nil
		}

		fn(d.Name(), data)
		return nil
	})
}

// authorizedKeys reads the keys directory. It's read on every login, so
// changes apply at once.
func (srv *Server) authorizedKeys() []ssh.PublicKey {
	var keys []ssh.PublicKey

	err := keyFiles(srv.cfg.KeysDirectory, func(name string, data []byte) {
		for len(data) > 0 {
			key, _, options, rest, err := ssh.ParseAuthorizedKey(data)
			if err != nil {
				return
			}
			data = rest

			// Options restrict a key, and pcompose can't enforce them.
			if len(options) > 0 {
				srv.logger.Warn("Ignoring a public key with options", "file", name, "fingerprint", ssh.FingerprintSHA256(key))
				continue
			}

			keys = append(keys, key)
		}
	})
	if err != nil {
		srv.logger.Error("Error reading the keys directory", "err", err)
	}

	return keys
}

// defaultHostKey is the host key pcompose generates if there are no others.
const defaultHostKey = "ssh_host_ed25519_key"

// loadHostKeys adds every private key in the private keys directory as a host
// key. A key replaces the ones of its type before it, so the default goes
// first: it's generated, and the others are there on purpose.
func (srv *Server) loadHostKeys(server *pssh.SSHServer) error {
	path := filepath.Join(srv.cfg.PrivateKeysDirectory, defaultHostKey)
	loaded := false

	addDefault := func() error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		key, err := srv.parseHostKey(data)
		if err != nil {
			return fmt.Errorf("%s: %w", defaultHostKey, err)
		}

		server.Config.AddHostKey(key)
		loaded = true

		return nil
	}

	// One that's there must load, since clients may know pcompose by it.
	if err := addDefault(); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	err := keyFiles(srv.cfg.PrivateKeysDirectory, func(name string, data []byte) {
		if name == defaultHostKey || strings.HasSuffix(name, ".pub") {
			return
		}

		key, err := srv.parseHostKey(data)
		if err != nil {
			srv.logger.Error("Skipping host key", "file", name, "err", err)
			return
		}

		server.Config.AddHostKey(key)
		loaded = true
	})
	if loaded || err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	if err := generateHostKey(path); err != nil {
		return err
	}

	return addDefault()
}

func (srv *Server) parseHostKey(data []byte) (ssh.Signer, error) {
	key, err := ssh.ParsePrivateKey(data)

	var encrypted *ssh.PassphraseMissingError
	if errors.As(err, &encrypted) && srv.cfg.PrivateKeyPassphrase != "" {
		return ssh.ParsePrivateKeyWithPassphrase(data, []byte(srv.cfg.PrivateKeyPassphrase))
	}

	return key, err
}

// generateHostKey writes a new key to path.
func generateHostKey(path string) error {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}

	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	return os.WriteFile(path, pem.EncodeToMemory(block), 0o600)
}
