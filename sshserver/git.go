package sshserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/antoniomika/pcompose/compose"
	"github.com/antoniomika/pcompose/hook"
	"github.com/antoniomika/pcompose/run"
	"github.com/picosh/pico/pkg/pssh"
)

var nameSegment = regexp.MustCompile(`^_*[A-Za-z0-9][A-Za-z0-9._-]*$`)

// checkName checks name is a project's: a path under the repositories that
// no flag or file outside them can be mistaken for.
func checkName(name string) error {
	for segment := range strings.SplitSeq(name, "/") {
		if !nameSegment.MatchString(segment) || strings.HasSuffix(segment, ".git") {
			return usagef("%q isn't a project name", name)
		}
	}

	return nil
}

// projectName is the project a repository's path names.
func projectName(repo string) string {
	return strings.TrimSuffix(strings.Trim(repo, "/"), ".git")
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func isExit(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit)
}

// exists reports whether there's a project named name. A project is its
// repository.
func (srv *Server) exists(name string) bool {
	return checkName(name) == nil && isDir(filepath.Join(srv.RepoPath(name), "objects"))
}

// projects lists the projects' names.
func (srv *Server) projects() ([]string, error) {
	root := filepath.Join(srv.cfg.DataDirectory, "repos")
	var names []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}

		rel, ok := strings.CutSuffix(path, ".git")
		if !ok {
			return nil
		}

		name, err := filepath.Rel(root, rel)
		if err == nil && srv.exists(filepath.ToSlash(name)) {
			names = append(names, filepath.ToSlash(name))
		}

		return fs.SkipDir
	})

	return names, err
}

// createProject makes the named project, with an empty repository and
// checkout.
func (srv *Server) createProject(ctx context.Context, name string) error {
	if err := checkName(name); err != nil {
		return err
	}

	srv.creating.Lock()
	defer srv.creating.Unlock()

	projects, err := srv.projects()
	if err != nil {
		return err
	}

	// A checkout can't hold another project's.
	for _, other := range projects {
		if strings.HasPrefix(other, name+"/") || strings.HasPrefix(name, other+"/") {
			return usagef("%s can't be created beside the project %s", name, other)
		}
	}

	if _, err := srv.git(ctx, "", "init", "--bare", "-q", "-b", "main", srv.RepoPath(name)); err != nil {
		return fmt.Errorf("create repository: %w", err)
	}

	return srv.ensureWorktree(ctx, name)
}

// ensureWorktree makes the project's checkout if it's missing. It's empty
// until a deploy fills it, so a .env can be put there first.
func (srv *Server) ensureWorktree(ctx context.Context, name string) error {
	worktree := srv.WorktreePath(name)
	if _, err := os.Stat(filepath.Join(worktree, ".git")); err == nil {
		return nil
	}

	if _, err := srv.git(ctx, "", "init", "-q", "-b", "main", worktree); err != nil {
		return fmt.Errorf("create checkout: %w", err)
	}

	return nil
}

// push is a push being received.
type push struct {
	project string

	mu sync.Mutex
	// accepted is set once pre-receive lets the push in, and reported once
	// post-receive is done, with err as why it failed. Git ignores that.
	accepted bool
	reported bool
	err      error
}

// result is why an accepted push didn't deploy, if it didn't.
func (p *push) result() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	switch {
	case p.err != nil:
		return usagef("%s was pushed, but didn't deploy", p.project)
	case p.accepted && !p.reported:
		return usagef("%s didn't deploy: its post-receive hook didn't run. Run 'redeploy %s' if it was pushed", p.project, p.project)
	}

	return nil
}

// serveGit serves a repository, creating its project on the first push.
func (srv *Server) serveGit(s *pssh.SSHServerConnSession, service, repo string) error {
	name := projectName(repo)

	exists := srv.exists(name)

	if service == uploadPack {
		if !exists {
			return usagef("no project named %s", name)
		}

		return run.Exec(s, run.Opts{Cmd: []string{"git", "upload-pack", srv.RepoPath(name)}, Stdin: true})
	}

	if !exists {
		if err := srv.createProject(s.Context(), name); err != nil {
			return err
		}
	}

	// Git skips hooks that are missing, which would store the push without
	// deploying it.
	if err := hook.Install(srv.hooksDir()); err != nil {
		return fmt.Errorf("install git hooks: %w", err)
	}

	b := make([]byte, 16)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)

	received := &push{project: name}
	srv.pushes.Store(id, received)
	defer srv.pushes.Delete(id)

	err := run.Exec(s, run.Opts{
		Cmd:   []string{"git", "-c", "core.hooksPath=" + srv.hooksDir(), "receive-pack", srv.RepoPath(name)},
		Env:   []string{hook.SocketEnv + "=" + srv.hookSocket, hook.IDEnv + "=" + id},
		Stdin: true,
	})
	if err != nil {
		return err
	}

	return received.result()
}

// serveHooks answers the git hooks of the pushes being received.
func (srv *Server) serveHooks(ctx context.Context) error {
	listener, err := srv.listenForHooks(ctx)
	if err != nil {
		return fmt.Errorf("listen for git hooks: %w", err)
	}
	srv.hookSocket = listener.Addr().String()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /{id}/{hook}", srv.handleHook)
	server := &http.Server{Handler: mux}

	context.AfterFunc(ctx, func() { _ = server.Close() })
	go func() {
		if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
			srv.logger.Error("Stopped listening for git hooks", "err", err)
		}
	}()

	return nil
}

// listenForHooks listens on a socket in the runtime directory. The data
// directory's path may be too long for one, or its filesystem may not support
// them, so by default a directory of pcompose's own is the fallback.
func (srv *Server) listenForHooks(ctx context.Context) (net.Listener, error) {
	socket := filepath.Join(srv.runtimeDir(), "hook.sock")
	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	listener, err := net.Listen("unix", socket)
	if err == nil || srv.cfg.RuntimeDirectory != "" {
		return listener, err
	}

	dir, mkErr := os.MkdirTemp("", "pcompose-")
	if mkErr != nil {
		return nil, errors.Join(err, mkErr)
	}
	context.AfterFunc(ctx, func() { _ = os.RemoveAll(dir) })

	return net.Listen("unix", filepath.Join(dir, "hook.sock"))
}

func (srv *Server) handleHook(w http.ResponseWriter, r *http.Request) {
	v, ok := srv.pushes.Load(r.PathValue("id"))
	if !ok {
		http.Error(w, "no such push", http.StatusNotFound)
		return
	}
	received := v.(*push)

	srv.work.Add(1)
	defer srv.work.Add(-1)

	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Trailer", hook.FailedTrailer)
	w.WriteHeader(http.StatusOK)

	s := run.NewDetached(r.Context(), &flushWriter{w, http.NewResponseController(w)})

	switch refs := parseRefUpdates(string(data)); r.PathValue("hook") {
	case "pre-receive":
		err = srv.preReceive(s, received.project)

		received.mu.Lock()
		received.accepted = err == nil
		received.mu.Unlock()
	case "post-receive":
		err = srv.postReceive(s, received.project, refs)

		received.mu.Lock()
		received.reported, received.err = true, err
		received.mu.Unlock()
	default:
		err = errors.New("unknown hook")
	}

	if err != nil {
		srv.logger.Error("Hook failed", "hook", r.PathValue("hook"), "project", received.project, "err", err)
		_, _ = fmt.Fprintf(s, "Error: %v\n", err)
		w.Header().Set(hook.FailedTrailer, "1")
	}
}

// flushWriter sends what it's written straight away.
type flushWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err == nil {
		err = f.rc.Flush()
	}

	return n, err
}

// refUpdate is one ref updated by a push.
type refUpdate struct {
	old, new, name string
}

func (r refUpdate) deleted() bool { return strings.Trim(r.new, "0") == "" }

// parseRefUpdates parses the "<old> <new> <ref>" lines git passes to
// pre-receive and post-receive hooks.
func parseRefUpdates(data string) []refUpdate {
	var refs []refUpdate

	for line := range strings.Lines(data) {
		if fields := strings.Fields(line); len(fields) == 3 {
			refs = append(refs, refUpdate{fields[0], fields[1], fields[2]})
		}
	}

	return refs
}

func (srv *Server) git(ctx context.Context, dir string, args ...string) (string, error) {
	return run.Output(ctx, run.Opts{Dir: dir, Cmd: append([]string{"git"}, args...)})
}

// deployBranch is the ref the project's repository HEAD points at.
func (srv *Server) deployBranch(ctx context.Context, name string) (string, error) {
	return srv.git(ctx, srv.RepoPath(name), "symbolic-ref", "HEAD")
}

func (srv *Server) refExists(ctx context.Context, name, ref string) bool {
	_, err := srv.git(ctx, srv.RepoPath(name), "show-ref", "--verify", "--quiet", ref)
	return err == nil
}

// worktreeChanges lists what was changed by hand in the project's checkout.
// Untracked files don't count: apps keep data in them, and a deploy leaves
// them alone.
func (srv *Server) worktreeChanges(ctx context.Context, name string) (string, error) {
	if err := srv.ensureWorktree(ctx, name); err != nil {
		return "", err
	}

	return srv.git(ctx, srv.WorktreePath(name), "status", "--porcelain", "--untracked-files=no")
}

func (srv *Server) preReceive(s run.Session, name string) error {
	changes, err := srv.worktreeChanges(s.Context(), name)
	if err != nil {
		return err
	}

	if changes != "" {
		return fmt.Errorf("the checkout has changes made by hand, which a deploy would discard. Undo them, or run 'redeploy %s --force' first:\n%s", name, changes)
	}

	return nil
}

func (srv *Server) postReceive(s run.Session, name string, refs []refUpdate) error {
	ctx := s.Context()

	branch, err := srv.deployBranch(ctx, name)
	if err != nil {
		return err
	}

	// The first branch pushed is deployed.
	if (branch == "refs/heads/main" || branch == "refs/heads/master") && !srv.refExists(ctx, name, branch) {
		for _, ref := range refs {
			if ref.deleted() || !strings.HasPrefix(ref.name, "refs/heads/") {
				continue
			}

			if _, err := srv.git(ctx, srv.RepoPath(name), "symbolic-ref", "HEAD", ref.name); err != nil {
				return err
			}

			branch = ref.name
			break
		}
	}

	for _, ref := range refs {
		if ref.name == branch && !ref.deleted() {
			return srv.deploy(s, name, false)
		}
	}

	_, _ = fmt.Fprintf(s, "Not deploying %s: it deploys %s, which this push didn't update.\n", name, shortRef(branch))

	return nil
}

func shortRef(ref string) string { return strings.TrimPrefix(ref, "refs/heads/") }

// lockProject waits until nothing else is deploying or deleting the project.
func (srv *Server) lockProject(s run.Session, name string) (unlock func(), err error) {
	return srv.deploys.Lock(s.Context(), name, func() {
		_, _ = fmt.Fprintf(s, "Waiting for another deploy of %s to finish...\n", name)
	})
}

// deploy checks out the project's branch and brings its apps in line with it.
// Once it starts, it finishes even if the client leaves.
func (srv *Server) deploy(s run.Session, name string, force bool) error {
	unlock, err := srv.lockProject(s, name)
	if err != nil {
		return fmt.Errorf("wait for other deploys of %s: %w", name, err)
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.Context()), srv.cfg.DeployTimeout)
	defer cancel()
	s = run.Keep(s, ctx)

	srv.work.Add(1)
	defer srv.work.Add(-1)

	srv.logger.Info("Deploying", "project", name)
	started := time.Now()

	d, reason, err := srv.checkout(s, name, force)
	if err != nil {
		return err
	}

	if reason != "" {
		srv.logger.Info("Not deploying", "project", name, "reason", reason)
		_, _ = fmt.Fprintf(s, "Not deploying %s: %s.\n", name, reason)
		return nil
	}

	if srv.cfg.IngressNetwork != "" {
		if err := compose.EnsureNetwork(ctx, srv.cfg.IngressNetwork); err != nil {
			return err
		}
	}

	err = d.Deploy(s, func(composeName string) error { return srv.record(ctx, name, composeName) })
	if err != nil {
		return err
	}

	srv.logger.Info("Deployed", "project", name, "took", time.Since(started).Round(time.Second))
	_, _ = fmt.Fprintf(s, "Deployed %s.\n", name)

	return nil
}

// checkout updates the project's checkout to its deployed branch. A reason
// says why there's nothing to deploy.
func (srv *Server) checkout(s run.Session, name string, force bool) (d *compose.Deployment, reason string, err error) {
	ctx := s.Context()
	worktree := srv.WorktreePath(name)

	branch, err := srv.deployBranch(ctx, name)
	if err != nil {
		return nil, "", err
	}

	if !srv.refExists(ctx, name, branch) {
		return nil, shortRef(branch) + " hasn't been pushed", nil
	}

	changes, err := srv.worktreeChanges(ctx, name)
	if err != nil {
		return nil, "", err
	}

	if changes != "" {
		if !force {
			return nil, fmt.Sprintf("the checkout has changes made by hand, which a deploy would discard. Run 'redeploy %s --force' to deploy anyway", name), nil
		}

		if _, err := srv.git(ctx, worktree, "reset", "-q", "--hard"); err != nil {
			return nil, "", fmt.Errorf("discard changes: %w", err)
		}
	}

	// Named by path, since a checkout moved from elsewhere has another origin.
	if _, err := srv.git(ctx, worktree, "fetch", "-q", "--prune", "--tags", "--force", srv.RepoPath(name), "+refs/heads/*:refs/remotes/origin/*"); err != nil {
		return nil, "", fmt.Errorf("fetch the repository: %w", err)
	}

	if _, err := srv.git(ctx, worktree, "checkout", "-q", "--no-track", "-B", shortRef(branch), "refs/remotes/origin/"+shortRef(branch)); err != nil {
		return nil, "", fmt.Errorf("check out %s: %w", shortRef(branch), err)
	}

	d = compose.New(name, worktree)
	if !d.HasFiles() {
		return nil, shortRef(branch) + " has no compose file", nil
	}

	return d, "", nil
}

// recordKey holds the compose projects a project has been deployed as, in its
// repository's git config.
const recordKey = "pcompose.deployed"

// record adds a compose project to the ones the project was deployed as.
func (srv *Server) record(ctx context.Context, name, composeName string) error {
	_, err := srv.git(ctx, srv.RepoPath(name), "config", "--replace-all", recordKey, composeName, "^"+regexp.QuoteMeta(composeName)+"$")
	return err
}

// recorded returns the compose projects the project was deployed as.
func (srv *Server) recorded(ctx context.Context, name string) []string {
	// Fails when there are none.
	out, err := srv.git(ctx, srv.RepoPath(name), "config", "--get-all", recordKey)
	if err != nil {
		return nil
	}

	return strings.Fields(out)
}

// delete removes a project's apps and volumes, its repository, and its
// checkout.
func (srv *Server) delete(s run.Session, name string) error {
	unlock, err := srv.lockProject(s, name)
	if err != nil {
		return err
	}
	defer unlock()

	projects, err := srv.projects()
	if err != nil {
		return err
	}

	others := map[string]bool{}
	for _, other := range projects {
		if other == name {
			continue
		}
		for _, composeName := range srv.recorded(s.Context(), other) {
			others[composeName] = true
		}
	}

	d := compose.New(name, srv.WorktreePath(name))
	if err := d.Remove(s, srv.recorded(s.Context(), name), others); err != nil {
		return fmt.Errorf("remove apps: %w", err)
	}

	// The repository goes last, since the project exists while it does.
	for _, dir := range []string{srv.WorktreePath(name), srv.RepoPath(name)} {
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}

	return nil
}
