package sshserver

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/antoniomika/pcompose/hook"
	"golang.org/x/crypto/ssh"
)

// TestMain runs git's hooks, which are links to this test binary.
func TestMain(m *testing.M) {
	if name := hook.Invoked(); name != "" {
		os.Exit(hook.Run(name))
	}

	os.Exit(m.Run())
}

// key is an SSH keypair in OpenSSH format.
type key struct{ Private, Public []byte }

func generateKey() (*key, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}

	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return nil, err
	}

	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, err
	}

	return &key{Private: pem.EncodeToMemory(block), Public: ssh.MarshalAuthorizedKey(sshPub)}, nil
}

// testServer is a running pcompose, and a client for it.
type testServer struct {
	t    *testing.T
	srv  *Server
	addr string
	key  string
	stop func()
}

func start(t *testing.T) *testServer {
	t.Helper()

	for _, name := range []string{"git", "ssh"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s isn't installed", name)
		}
	}

	// Short, for sockets' paths.
	dir, err := os.MkdirTemp("", "pc")
	if err != nil {
		t.Fatal(err)
	}

	key, err := generateKey()
	if err != nil {
		t.Fatal(err)
	}

	pubkeys := filepath.Join(dir, "pubkeys")
	if err := os.MkdirAll(pubkeys, 0o700); err != nil {
		t.Fatal(err)
	}

	for path, data := range map[string][]byte{
		filepath.Join(dir, "id"):        key.Private,
		filepath.Join(pubkeys, "alice"): key.Public,
		filepath.Join(pubkeys, ".keep"): nil,
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()

	srv, err := New(Config{
		SSHAddress:           addr,
		DataDirectory:        filepath.Join(dir, "data"),
		PrivateKeysDirectory: filepath.Join(dir, "keys"),
		KeysDirectory:        pubkeys,
		DeployTimeout:        5 * time.Minute,
		ShutdownTimeout:      30 * time.Second,
	}, slog.New(slog.NewTextHandler(t.Output(), nil)))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	ts := &testServer{t: t, srv: srv, addr: addr, key: filepath.Join(dir, "id")}

	stopped := false
	ts.stop = func() {
		if stopped {
			return
		}
		stopped = true

		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run() = %v", err)
		}
	}

	t.Cleanup(func() {
		ts.stop()
		_ = os.RemoveAll(dir)
	})

	for range 100 {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			_ = conn.Close()
			return ts
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatal("the server didn't start")
	return nil
}

func (ts *testServer) sshArgs() []string {
	_, port, _ := net.SplitHostPort(ts.addr)
	return []string{"-F", "/dev/null", "-i", ts.key, "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR", "-p", port}
}

// ssh runs a command as target.
func (ts *testServer) ssh(target string, args ...string) (string, error) {
	cmd := exec.Command("ssh", append(append(ts.sshArgs(), target+"@127.0.0.1"), args...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (ts *testServer) mustSSH(target string, args ...string) string {
	ts.t.Helper()

	out, err := ts.ssh(target, args...)
	if err != nil {
		ts.t.Fatalf("ssh %s %v: %v\n%s", target, args, err, out)
	}
	return out
}

// repo is a client's repository.
type repo struct {
	ts  *testServer
	dir string
}

func (ts *testServer) repo() *repo {
	r := &repo{ts: ts, dir: ts.t.TempDir()}
	r.mustGit("init", "-q", "-b", "main")
	return r
}

func (r *repo) git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(),
		"GIT_SSH_COMMAND=ssh "+strings.Join(r.ts.sshArgs(), " "),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@example.com", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@example.com",
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (r *repo) mustGit(args ...string) string {
	r.ts.t.Helper()

	out, err := r.git(args...)
	if err != nil {
		r.ts.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return out
}

func (r *repo) commit(files map[string]string) {
	r.ts.t.Helper()

	for name, content := range files {
		if err := os.WriteFile(filepath.Join(r.dir, name), []byte(content), 0o600); err != nil {
			r.ts.t.Fatal(err)
		}
	}
	r.mustGit("add", "-A")
	r.mustGit("commit", "-qm", "change")
}

func (r *repo) push(project string, refs ...string) (string, error) {
	return r.git(append([]string{"push", "ssh://git@" + r.ts.addr + "/" + project + ".git"}, refs...)...)
}

func contains(t *testing.T, what, out, want string) {
	t.Helper()

	if !strings.Contains(out, want) {
		t.Errorf("%s = %q, want it to contain %q", what, out, want)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestPush(t *testing.T) {
	ts := start(t)
	r := ts.repo()

	r.commit(map[string]string{"file": "one"})
	out, err := r.push("lib", "main")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	contains(t, "push", out, "Not deploying lib: main has no compose file")

	file := filepath.Join(ts.srv.WorktreePath("lib"), "file")
	if got := read(t, file); got != "one" {
		t.Errorf("checkout holds %q, want one", got)
	}

	// Other branches are stored without deploying.
	r.mustGit("checkout", "-qb", "next")
	r.commit(map[string]string{"file": "two"})
	out, _ = r.push("lib", "next")
	contains(t, "push", out, "it deploys main")
	if got := read(t, file); got != "one" {
		t.Errorf("checkout holds %q after pushing another branch, want one", got)
	}

	ts.mustSSH("x", "git", "-C", "../repos/lib.git", "symbolic-ref", "HEAD", "refs/heads/next")
	ts.mustSSH("lib", "redeploy")
	if got := read(t, file); got != "two" {
		t.Errorf("checkout holds %q after changing branch, want two", got)
	}

	// Files an app writes don't stop a push, and survive it.
	data := filepath.Join(ts.srv.WorktreePath("lib"), "data")
	if err := os.WriteFile(data, []byte("app"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.commit(map[string]string{"file": "three"})
	if out, err := r.push("lib", "next"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if got := read(t, file) + read(t, data); got != "threeapp" {
		t.Errorf("checkout holds %q, want threeapp", got)
	}

	// Changes made by hand do, until they're discarded.
	if err := os.WriteFile(file, []byte("hand"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.commit(map[string]string{"file": "four"})
	if out, err := r.push("lib", "next"); err == nil {
		t.Errorf("push over changes made by hand succeeded:\n%s", out)
	}
	contains(t, "redeploy", ts.mustSSH("lib", "redeploy"), "changes made by hand")
	ts.mustSSH("lib", "redeploy", "--force")
	if out, err := r.push("lib", "next"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if got := read(t, file); got != "four" {
		t.Errorf("checkout holds %q, want four", got)
	}

	clone := ts.repo()
	clone.mustGit("pull", "-q", "ssh://git@"+ts.addr+"/lib", "next")
	if got := read(t, filepath.Join(clone.dir, "file")); got != "four" {
		t.Errorf("clone holds %q, want four", got)
	}

	contains(t, "ls", ts.mustSSH("lib", "ls"), "file")

	if out, err := ts.ssh("lib", "delete"); err == nil {
		t.Errorf("delete without --confirm succeeded: %s", out)
	}
	ts.mustSSH("lib", "delete", "--confirm", "lib")
	if isDir(ts.srv.RepoPath("lib")) || isDir(ts.srv.WorktreePath("lib")) {
		t.Error("delete left the repository or checkout")
	}
	contains(t, "projects", ts.mustSSH("x", "projects"), "No projects yet")
}

func TestNames(t *testing.T) {
	ts := start(t)
	r := ts.repo()
	r.commit(map[string]string{"file": "one"})

	for _, name := range []string{"../evil", "a/../../evil", ".hidden", "-flag", "a b", "a.git/hooks", "_"} {
		if out, err := r.push(strings.ReplaceAll(name, " ", "%20"), "main"); err == nil {
			t.Errorf("push to %q succeeded:\n%s", name, out)
		}
	}

	if out, err := r.push("team/web", "main"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if out, err := ts.ssh("x", "create", "team"); err == nil {
		t.Errorf("create of a project around another's checkout succeeded: %s", out)
	}
	if out, err := ts.ssh("x", "create", "team/web/api"); err == nil {
		t.Errorf("create of a project in another's checkout succeeded: %s", out)
	}
	ts.mustSSH("x", "create", "team/api")
}

// TestDotEnv checks a project's .env, which isn't committed, can be there
// before its first push and survives deploys.
func TestDotEnv(t *testing.T) {
	ts := start(t)
	ts.mustSSH("x", "create", "web")
	contains(t, "projects", ts.mustSSH("x", "projects"), "web\n")

	put := exec.Command("ssh", append(append(ts.sshArgs(), "web@127.0.0.1"), "tee", ".env")...)
	put.Stdin = strings.NewReader("TOKEN=secret\n")
	if out, err := put.CombinedOutput(); err != nil {
		t.Fatalf("tee .env: %v: %s", err, out)
	}

	dotEnv := filepath.Join(ts.srv.WorktreePath("web"), ".env")
	check := func(when string) {
		t.Helper()
		if got := read(t, dotEnv); got != "TOKEN=secret\n" {
			t.Errorf(".env after %s = %q", when, got)
		}
	}

	if info, err := os.Stat(ts.srv.projectsDir()); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("projects directory = %v, %v, want mode 0700", info, err)
	}

	r := ts.repo()
	r.commit(map[string]string{"file": "one", ".gitignore": ".env\n"})
	if out, err := r.push("web", "main"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	check("the first push")

	file := filepath.Join(ts.srv.WorktreePath("web"), "file")
	if err := os.WriteFile(file, []byte("hand"), 0o600); err != nil {
		t.Fatal(err)
	}
	ts.mustSSH("web", "redeploy", "--force")
	check("a forced redeploy")
	if got := read(t, file); got != "one" {
		t.Errorf("checkout holds %q after a forced redeploy, want one", got)
	}
}

func TestExec(t *testing.T) {
	ts := start(t)

	out, err := ts.ssh("x", "sh", "-c", "'exit 7'")
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 7 {
		t.Errorf("exit 7 = %v (%s)", err, out)
	}

	cmd := exec.Command("ssh", append(append(ts.sshArgs(), "x@127.0.0.1"), "cat")...)
	input := bytes.Repeat([]byte("0123456789abcdef"), 1<<16)
	cmd.Stdin = bytes.NewReader(input)
	got, err := cmd.Output()
	if err != nil || !bytes.Equal(got, input) {
		t.Errorf("cat returned %d bytes, %v, want %d", len(got), err, len(input))
	}

	contains(t, "tty", ts.mustSSHTTY("x", "tty"), "/dev/")

	// What a command leaves in the background ends with its client too, though
	// it holds the output open.
	cmd = exec.Command("ssh", append(append(ts.sshArgs(), "x@127.0.0.1"), "sh", "-c", "'sleep 3599 &'")...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the session to start", func() bool { return ts.srv.active.busy() == 1 })
	time.Sleep(500 * time.Millisecond)
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	waitFor(t, "the session to end", func() bool { return ts.srv.active.busy() == 0 })

	// A command ends with its client.
	marker := filepath.Join(t.TempDir(), "pid")
	cmd = exec.Command("ssh", append(append(ts.sshArgs(), "x@127.0.0.1"), "sh", "-c", fmt.Sprintf("'echo $$ > %s; sleep 60'", marker))...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the command to start", func() bool { data, _ := os.ReadFile(marker); return len(data) > 0 })
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	pid := strings.TrimSpace(read(t, marker))
	waitFor(t, "the command to end", func() bool { return exec.Command("kill", "-0", pid).Run() != nil })
}

func (ts *testServer) mustSSHTTY(target string, args ...string) string {
	ts.t.Helper()

	cmd := exec.Command("ssh", append(append(ts.sshArgs(), "-tt", target+"@127.0.0.1"), args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		ts.t.Fatalf("ssh -tt %s %v: %v\n%s", target, args, err, out)
	}
	return string(out)
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()

	for range 200 {
		if ok() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestAuthentication(t *testing.T) {
	ts := start(t)

	other, err := generateKey()
	if err != nil {
		t.Fatal(err)
	}

	signer, err := ssh.ParsePrivateKey(other.Private)
	if err != nil {
		t.Fatal(err)
	}

	dial := func(auth ...ssh.AuthMethod) error {
		client, err := ssh.Dial("tcp", ts.addr, &ssh.ClientConfig{User: "alice", Auth: auth, HostKeyCallback: ssh.InsecureIgnoreHostKey()})
		if err == nil {
			_ = client.Close()
		}
		return err
	}

	if dial() == nil || dial(ssh.Password("")) == nil || dial(ssh.PublicKeys(signer)) == nil {
		t.Error("logged in without an authorized key")
	}

	// An entry that can't be read doesn't lock the others out.
	for _, name := range []string{"aaa", "zzz"} {
		if err := os.Symlink(t.TempDir(), filepath.Join(ts.srv.cfg.KeysDirectory, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := dial(ssh.PublicKeys(signerFor(t, ts.key))); err != nil {
		t.Errorf("an unreadable key file locked out another: %v", err)
	}

	// A restricted key doesn't become an unrestricted one.
	file := filepath.Join(ts.srv.cfg.KeysDirectory, "other")
	if err := os.WriteFile(file, append([]byte(`command="true" `), other.Public...), 0o600); err != nil {
		t.Fatal(err)
	}
	if dial(ssh.PublicKeys(signer)) == nil {
		t.Error("logged in with a key that has options")
	}

	if err := os.WriteFile(file, other.Public, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := dial(ssh.PublicKeys(signer)); err != nil {
		t.Errorf("a key added while running wasn't accepted: %v", err)
	}
}

func signerFor(t *testing.T, path string) ssh.Signer {
	t.Helper()

	signer, err := ssh.ParsePrivateKey([]byte(read(t, path)))
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// TestDrain checks a stopping pcompose ends shells at once, turns new sessions
// away, and waits for commands.
func TestDrain(t *testing.T) {
	ts := start(t)

	shell := exec.Command("ssh", append(append(ts.sshArgs(), "-tt", "x@127.0.0.1"), "sh", "-c", "'echo ready; sleep 60'")...)
	// Held open, so the shell doesn't end with its input.
	if _, err := shell.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	var shellOut bytes.Buffer
	shell.Stdout, shell.Stderr = &shellOut, &shellOut
	if err := shell.Start(); err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(t.TempDir(), "started")
	busy := make(chan error, 1)
	go func() {
		_, err := ts.ssh("x", "sh", "-c", fmt.Sprintf("'touch %s; sleep 3'", marker))
		busy <- err
	}()
	waitFor(t, "the command to start", func() bool { _, err := os.Stat(marker); return err == nil })

	stopped := make(chan struct{})
	started := time.Now()
	go func() {
		ts.stop()
		close(stopped)
	}()

	if err := shell.Wait(); err == nil || time.Since(started) > 2*time.Second {
		t.Errorf("the shell ended after %s with %v, want it ended at once", time.Since(started), err)
	}
	contains(t, "shell", shellOut.String(), "pcompose is stopping")

	if out, err := ts.ssh("x", "projects"); err == nil {
		t.Errorf("a session started while stopping: %s", out)
	}

	if err := <-busy; err != nil {
		t.Errorf("a running command was interrupted: %v", err)
	}
	<-stopped
	if time.Since(started) < 2*time.Second {
		t.Error("pcompose stopped before its command finished")
	}
}

const testCompose = `services:
  web:
    image: traefik/whoami
    environment: [VERSION=%s, "TOKEN=${TOKEN}"]
    depends_on: [db]
  db:
    image: traefik/whoami
%s`

// TestDeployIntegration pushes projects that deploy on this machine's docker
// engine.
func TestDeployIntegration(t *testing.T) {
	if os.Getenv("PCOMPOSE_INTEGRATION") != "1" {
		t.Skip("set PCOMPOSE_INTEGRATION=1 to deploy on this machine's docker engine")
	}

	ts := start(t)
	name := fmt.Sprintf("pcompose-test-%d", time.Now().UnixNano())

	t.Cleanup(func() {
		for _, name := range []string{name, name + "b"} {
			_ = exec.Command("docker", "compose", "-p", name, "down", "--volumes", "--remove-orphans").Run()
		}
	})

	containers := func(args ...string) []string {
		t.Helper()

		out, err := exec.Command("docker", append([]string{"ps", "-q", "--no-trunc", "--filter", "label=com.docker.compose.project=" + name}, args...)...).Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.Fields(string(out))
	}
	web := func() []string { return containers("--filter", "label=com.docker.compose.service=web") }

	ts.mustSSH("x", "create", name)
	if err := os.WriteFile(filepath.Join(ts.srv.WorktreePath(name), ".env"), []byte("TOKEN=hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := ts.repo()
	deploy := func(version, extra string) (string, error) {
		r.commit(map[string]string{"compose.yaml": fmt.Sprintf(testCompose, version, extra)})
		out, err := r.push(name, "main")
		t.Logf("push %s: %v\n%s", version, err, out)
		return out, err
	}

	out, err := deploy("1", "  gone:\n    image: traefik/whoami\n")
	if err != nil {
		t.Fatal(err)
	}
	contains(t, "push", out, "remote: Deployed "+name)

	first := web()
	if n := len(containers()); len(first) != 1 || n != 3 {
		t.Fatalf("web runs %d containers of %d, want 1 of 3", len(first), n)
	}

	inspect, err := exec.Command("docker", "inspect", "--format", "{{.HostConfig.RestartPolicy.Name}} {{.Config.Env}}", first[0]).Output()
	if err != nil {
		t.Fatal(err)
	}
	contains(t, "inspect", string(inspect), "unless-stopped")
	contains(t, "inspect", string(inspect), "TOKEN=hunter2")

	// A push replaces what changed, and removes services that are gone.
	if _, err := deploy("2", ""); err != nil {
		t.Fatal(err)
	}
	second := web()
	if n := len(containers("-a")); len(second) != 1 || second[0] == first[0] || n != 2 {
		t.Fatalf("web runs %v of %d containers, want one new container in place of %v, of 2", second, n, first)
	}

	ts.mustSSH(name, "redeploy")
	if again := web(); len(again) != 1 || again[0] != second[0] {
		t.Errorf("an unchanged deploy replaced web: %v, want %v", again, second)
	}

	out, err = deploy("3", "  broken: {image: 1.2.3.4.invalid/none}\n")
	if err == nil {
		t.Error("push succeeded though its deploy failed")
	}
	contains(t, "push", out, "didn't deploy")

	if out, err := deploy("4", ""); err != nil {
		t.Fatalf("push after a failed deploy: %v\n%s", err, out)
	}

	contains(t, "compose ps", ts.mustSSH("x", "compose", name, "ps"), name+"-db-1")
	contains(t, "logs", ts.mustSSH("x", "logs", name, "db"), "db-1")
	contains(t, "compose", ts.mustSSH(name, "compose", "ps", "--services"), "web")
	contains(t, "exec", ts.mustSSH("c-"+name+"-db-1", "/whoami", "--help"), "-port")

	_, port, _ := net.SplitHostPort(ts.addr)
	dockerSSH := exec.Command("docker", "-H", "ssh://x@127.0.0.1:"+port, "ps", "--format", "{{.Names}}")
	dockerSSH.Env = append(os.Environ(), "PATH="+sshWrapper(t, ts)+":"+os.Getenv("PATH"))
	if out, err := dockerSSH.CombinedOutput(); err != nil {
		t.Errorf("docker over ssh: %v: %s", err, out)
	} else {
		contains(t, "docker over ssh", string(out), name+"-db-1")
	}

	// Nor can a command's own flags, which come after the check, name others.
	for _, args := range [][]string{{"-p", "other", "down"}, {"--profile", "x", "-p", "other", "down"}, {"-f", "/dev/null", "ps"}} {
		out, err := ts.ssh("x", append([]string{"compose", name}, args...)...)
		if err == nil {
			t.Errorf("compose %v succeeded", args)
		}
		contains(t, "compose", out, "aren't supported here")
	}

	// Another project can't take over this one's containers.
	ts.mustSSH("x", "create", name+"b")
	if err := os.WriteFile(filepath.Join(ts.srv.WorktreePath(name+"b"), ".env"), []byte("COMPOSE_PROJECT_NAME="+name+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := ts.repo()
	other.commit(map[string]string{"compose.yaml": "services: {db: {image: traefik/whoami}}\n"})
	out, err = other.push(name+"b", "main")
	if err == nil {
		t.Error("a project deployed over another's")
	}
	contains(t, "push", out, "already runs from")
	if out, err := ts.ssh("x", "compose", name+"b", "down"); err == nil {
		t.Errorf("a project took another's down: %s", out)
	}
	ts.mustSSH("x", "delete", name+"b", "--confirm", name+"b")
	if n := len(containers()); n != 2 {
		t.Errorf("%d containers run, want 2", n)
	}

	ts.mustSSH("x", "compose", name, "down")
	if left := containers("-a"); len(left) != 0 {
		t.Errorf("down left %v", left)
	}
	ts.mustSSH("x", "redeploy", name)
	ts.mustSSH("x", "delete", name, "--confirm", name)
	if left := containers("-a"); len(left) != 0 {
		t.Errorf("delete left %v", left)
	}
}

// TestDeployEdgesIntegration checks deploys and deletes that don't go by the
// checkout's compose files.
func TestDeployEdgesIntegration(t *testing.T) {
	if os.Getenv("PCOMPOSE_INTEGRATION") != "1" {
		t.Skip("set PCOMPOSE_INTEGRATION=1 to deploy on this machine's docker engine")
	}

	ts := start(t)

	project := func(suffix string) string {
		name := fmt.Sprintf("pcompose-%s-%d", suffix, time.Now().UnixNano())
		t.Cleanup(func() {
			_ = exec.Command("docker", "compose", "-p", name, "down", "--volumes", "--remove-orphans", "--rmi", "local").Run()
		})
		return name
	}
	list := func(name string, args ...string) string {
		t.Helper()
		out, err := exec.Command("docker", append(args, "--filter", "label=com.docker.compose.project="+name)...).Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	mustPush := func(r *repo, name string) {
		t.Helper()
		if out, err := r.push(name, "main"); err != nil {
			t.Fatalf("push: %v\n%s", err, out)
		}
	}

	t.Run("subdirectory", func(t *testing.T) {
		name := project("sub")
		r := ts.repo()
		if err := os.Mkdir(filepath.Join(r.dir, "ops"), 0o700); err != nil {
			t.Fatal(err)
		}
		r.commit(map[string]string{
			"ops/compose.yaml": "name: " + name + "\nservices: {web: {image: traefik/whoami}}\n",
			".env":             "DIR=ops\nCOMPOSE_FILE=${DIR}/compose.yaml # production\n",
		})
		mustPush(r, name)
		ts.mustSSH(name, "redeploy")
		contains(t, "compose ps", ts.mustSSH(name, "compose", "ps"), name+"-web-1")
		ts.mustSSH(name, "delete", "--confirm", name)
		if left := list(name, "ps", "-aq"); left != "" {
			t.Errorf("delete left %s", left)
		}
	})

	t.Run("nested name", func(t *testing.T) {
		stamp := fmt.Sprint(time.Now().UnixNano())
		name := "team/my." + stamp
		composeName := "team_my" + stamp
		t.Cleanup(func() {
			_ = exec.Command("docker", "compose", "-p", composeName, "down", "--volumes").Run()
		})

		r := ts.repo()
		r.commit(map[string]string{"compose.yaml": "services: {web: {image: traefik/whoami}}\n"})
		mustPush(r, name)
		if list(composeName, "ps", "-q") == "" {
			t.Errorf("%s isn't running", composeName)
		}
		ts.mustSSH("x", "delete", name, "--confirm", name)
		if left := list(composeName, "ps", "-aq"); left != "" {
			t.Errorf("delete left %s", left)
		}
	})

	t.Run("compose without files", func(t *testing.T) {
		name := project("nofiles")
		r := ts.repo()
		r.commit(map[string]string{"compose.yaml": "name: " + name + "\nservices: {web: {image: traefik/whoami}}\n"})
		mustPush(r, "nofiles")
		r.mustGit("rm", "-q", "compose.yaml")
		r.mustGit("commit", "-qm", "gone", "--allow-empty")
		mustPush(r, "nofiles")
		ts.mustSSH("nofiles", "compose", "down")
		if left := list(name, "ps", "-aq"); left != "" {
			t.Errorf("compose down left %s", left)
		}
	})

	t.Run("delete without compose files", func(t *testing.T) {
		name := project("gone")
		r := ts.repo()
		r.commit(map[string]string{"compose.yaml": "services: {web: {image: traefik/whoami, volumes: ['data:/d']}}\nvolumes: {data: {}}\n"})
		mustPush(r, name)
		r.mustGit("rm", "-q", "compose.yaml")
		r.mustGit("commit", "-qm", "gone", "--allow-empty")
		mustPush(r, name)
		ts.mustSSH("x", "delete", name, "--confirm", name)
		if left := list(name, "ps", "-aq") + list(name, "volume", "ls", "-q") + list(name, "network", "ls", "-q"); left != "" {
			t.Errorf("delete left %s", left)
		}
	})

	t.Run("delete after down", func(t *testing.T) {
		withVolume := "services: {web: {image: traefik/whoami, volumes: ['data:/d']}}\nvolumes: {data: {}}\n"

		for _, now := range []string{"same", "gone", "invalid"} {
			name := project("down-" + now)
			r := ts.repo()
			r.commit(map[string]string{"compose.yaml": withVolume})
			mustPush(r, name)
			ts.mustSSH(name, "compose", "down")

			switch now {
			case "gone":
				r.mustGit("rm", "-q", "compose.yaml")
				r.mustGit("commit", "-qm", "gone", "--allow-empty")
				mustPush(r, name)
			case "invalid":
				r.commit(map[string]string{"compose.yaml": withVolume + "x: ${TAG:?set TAG}\n"})
				if out, err := r.push(name, "main"); err == nil {
					t.Errorf("push of an invalid compose file succeeded: %s", out)
				}
			}

			if list(name, "volume", "ls", "-q") == "" {
				t.Fatalf("%s: there's no volume to delete", now)
			}
			ts.mustSSH("x", "delete", name, "--confirm", name)
			if left := list(name, "volume", "ls", "-q"); left != "" {
				t.Errorf("%s: delete left %s", now, left)
			}
		}
	})

	t.Run("delete beside another's volumes", func(t *testing.T) {
		name := project("shared")
		r := ts.repo()
		r.commit(map[string]string{"compose.yaml": "name: " + name + "\nservices: {web: {image: traefik/whoami, volumes: ['data:/d']}}\nvolumes: {data: {}}\n"})
		mustPush(r, "shared-a")
		ts.mustSSH("shared-a", "compose", "down")
		mustPush(r, "shared-b")
		ts.mustSSH("shared-b", "compose", "down")

		contains(t, "delete", ts.mustSSH("shared-b", "delete", "--confirm", "shared-b"), "Leaving")
		if list(name, "volume", "ls", "-q") == "" {
			t.Error("delete removed a volume another project uses")
		}
		ts.mustSSH("shared-a", "delete", "--confirm", "shared-a")
		if left := list(name, "volume", "ls", "-q"); left != "" {
			t.Errorf("delete left %s", left)
		}
	})

	t.Run("compose exec in a terminal", func(t *testing.T) {
		name := project("exec")
		r := ts.repo()
		r.commit(map[string]string{"compose.yaml": "services: {web: {image: 'alpine:3', command: sleep 1d, init: true}}\n"})
		mustPush(r, name)

		shell := exec.Command("ssh", append(append(ts.sshArgs(), "-tt", "x@127.0.0.1"), "compose", name, "exec", "web", "sh")...)
		stdin, err := shell.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := shell.Start(); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the shell", func() bool {
			ts.srv.active.mu.Lock()
			defer ts.srv.active.mu.Unlock()
			return len(ts.srv.active.sessions) == 1
		})
		if n := ts.srv.active.busy(); n != 0 {
			t.Errorf("%d sessions are busy, want the shell to be interactive", n)
		}
		_ = stdin.Close()
		_ = shell.Process.Kill()
		_ = shell.Wait()
		ts.mustSSH("x", "delete", name, "--confirm", name)
	})

	t.Run("invalid compose file", func(t *testing.T) {
		name := project("invalid")
		r := ts.repo()
		r.commit(map[string]string{"compose.yaml": "services: {web: {image: traefik/whoami}}\n"})
		mustPush(r, name)
		r.commit(map[string]string{"compose.yaml": "services: {web: {image: 'traefik/whoami:${TAG:?set TAG}'}}\n"})
		if out, err := r.push(name, "main"); err == nil {
			t.Errorf("push of an invalid compose file succeeded: %s", out)
		}

		for _, args := range [][]string{{"redeploy"}, {"compose", "ps"}} {
			out, err := ts.ssh(name, args...)
			if err == nil {
				t.Errorf("%v with an invalid compose file succeeded", args)
			}
			contains(t, args[0], out, "set TAG")
		}

		ts.mustSSH("x", "delete", name, "--confirm", name)
		if left := list(name, "ps", "-aq"); left != "" {
			t.Errorf("delete left %s", left)
		}
	})

	t.Run("one-off containers", func(t *testing.T) {
		name := project("oneoff")
		r := ts.repo()
		r.commit(map[string]string{"compose.yaml": "services: {web: {image: 'alpine:3', command: sleep 1d, init: true}}\n"})
		mustPush(r, name)
		ts.mustSSH("x", "compose", name, "run", "-d", "--name", name+"-job", "web", "sleep", "600")
		ts.mustSSH(name, "redeploy")

		for container, want := range map[string]string{name + "-job": "no", name + "-web-1": "unless-stopped"} {
			out, err := exec.Command("docker", "inspect", "--format", "{{.HostConfig.RestartPolicy.Name}}", container).Output()
			if err != nil || strings.TrimSpace(string(out)) != want {
				t.Errorf("%s's restart policy = %s, %v, want %s", container, out, err, want)
			}
		}

		ts.mustSSH("x", "delete", name, "--confirm", name)
		if left := list(name, "ps", "-aq"); left != "" {
			t.Errorf("delete left %s", left)
		}
	})

	t.Run("disconnect", func(t *testing.T) {
		name := project("gone-client")
		r := ts.repo()
		r.commit(map[string]string{
			"Dockerfile":   "FROM alpine:3\nRUN sleep 5 && echo " + name + "\nCMD sleep 1d\n",
			"compose.yaml": "services: {web: {build: ., init: true}}\n",
		})

		cmd := exec.Command("git", "push", "ssh://git@"+ts.addr+"/"+name+".git", "main")
		cmd.Dir = r.dir
		cmd.Env = append(os.Environ(), "GIT_SSH_COMMAND=ssh "+strings.Join(ts.sshArgs(), " "))
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the checkout", func() bool {
			_, err := os.Stat(filepath.Join(ts.srv.WorktreePath(name), "Dockerfile"))
			return err == nil
		})
		time.Sleep(time.Second)
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()

		for range 60 {
			if list(name, "ps", "-q") != "" {
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
		t.Error("the deploy didn't finish after its client left")
	})
}

// sshWrapper is a directory holding an ssh with the test's options, which
// docker doesn't pass.
func sshWrapper(t *testing.T, ts *testServer) string {
	t.Helper()

	dir := t.TempDir()
	script := "#!/bin/sh\nexec /usr/bin/ssh " + strings.Join(ts.sshArgs(), " ") + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestHooks checks a push whose hooks can't run fails, or has them restored.
func TestHooks(t *testing.T) {
	ts := start(t)
	r := ts.repo()
	r.commit(map[string]string{"file": "one"})
	file := filepath.Join(ts.srv.WorktreePath("lib"), "file")

	if err := os.RemoveAll(ts.srv.hooksDir()); err != nil {
		t.Fatal(err)
	}
	if out, err := r.push("lib", "main"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if got := read(t, file); got != "one" {
		t.Errorf("checkout holds %q after a push whose hooks were missing, want one", got)
	}

	// Nothing to push runs no hooks, and is fine.
	if out, err := r.push("lib", "main"); err != nil {
		t.Errorf("push of nothing: %v\n%s", err, out)
	}

	// A hook that lets the push in, then goes missing.
	p := &push{project: "lib", accepted: true}
	if p.result() == nil {
		t.Error("a push whose post-receive never ran succeeded")
	}
	p.reported = true
	if err := p.result(); err != nil {
		t.Errorf("a push that deployed failed: %v", err)
	}

	if err := os.Remove(filepath.Join(ts.srv.runtimeDir(), "hook.sock")); err != nil {
		t.Fatal(err)
	}
	r.commit(map[string]string{"file": "two"})
	if out, err := r.push("lib", "main"); err == nil {
		t.Errorf("push without the hooks' socket succeeded:\n%s", out)
	}
}

// TestLongDataDirectory checks hooks work when the data directory's path is
// too long for a socket.
func TestLongDataDirectory(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := &Server{cfg: Config{DataDirectory: filepath.Join(t.TempDir(), strings.Repeat("long", 40))}}
	if err := os.MkdirAll(srv.runtimeDir(), 0o700); err != nil {
		t.Fatal(err)
	}

	listener, err := srv.listenForHooks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = listener.Close()

	srv.cfg.RuntimeDirectory = srv.runtimeDir()
	if listener, err := srv.listenForHooks(ctx); err == nil {
		_ = listener.Close()
		t.Error("listened on a path that's too long")
	}
}

func TestHostKeys(t *testing.T) {
	generate := func() *key {
		t.Helper()
		k, err := generateKey()
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	encrypted := func(passphrase string) []byte {
		t.Helper()
		raw, err := ssh.ParseRawPrivateKey(generate().Private)
		if err != nil {
			t.Fatal(err)
		}
		block, err := ssh.MarshalPrivateKeyWithPassphrase(raw, "", []byte(passphrase))
		if err != nil {
			t.Fatal(err)
		}
		return pem.EncodeToMemory(block)
	}

	mine, other, locked := generate(), generate(), encrypted("pw")

	tests := []struct {
		name     string
		files    map[string][]byte
		readOnly bool
		// offered is the key clients see, or nil for a generated one.
		offered []byte
		fails   bool
	}{
		{name: "none"},
		{name: "default", files: map[string][]byte{defaultHostKey: mine.Private}, offered: mine.Private},
		{name: "encrypted default", files: map[string][]byte{defaultHostKey: locked}, offered: locked},
		{name: "default with another passphrase", files: map[string][]byte{defaultHostKey: encrypted("no"), "id_ed25519": mine.Private}, fails: true},
		{name: "sorts before the default", files: map[string][]byte{"id_ed25519": mine.Private}, offered: mine.Private},
		{name: "sorts after the default", files: map[string][]byte{"ssh_key": mine.Private}, offered: mine.Private},
		{name: "beside a default", files: map[string][]byte{"id_ed25519": mine.Private, defaultHostKey: other.Private}, offered: mine.Private},
		{name: "read-only", files: map[string][]byte{"id_ed25519": mine.Private}, readOnly: true, offered: mine.Private},
		{name: "unusable", files: map[string][]byte{"extra": encrypted("no"), "extra.pub": mine.Public}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, data := range tt.files {
				if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tt.readOnly {
				if err := os.Chmod(dir, 0o500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
			}

			srv := &Server{cfg: Config{SSHAddress: "127.0.0.1:0", PrivateKeysDirectory: dir, PrivateKeyPassphrase: "pw"}, logger: slog.New(slog.DiscardHandler)}
			server, err := srv.newSSHServer(t.Context())
			if tt.fails {
				if err == nil {
					t.Error("started without the default host key")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}

			want := tt.offered
			if want == nil {
				path := filepath.Join(dir, defaultHostKey)
				want = []byte(read(t, path))
				if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
					t.Errorf("generated key = %v, %v, want mode 0600", info, err)
				}
			}
			signer, err := srv.parseHostKey(want)
			if err != nil {
				t.Fatal(err)
			}

			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = listener.Close() }()
			go func() {
				if conn, err := listener.Accept(); err == nil {
					_, _, _, _ = ssh.NewServerConn(conn, server.Config.ServerConfig)
					_ = conn.Close()
				}
			}()

			// It has no key to log in with, but sees the server's first.
			var offered ssh.PublicKey
			_, _ = ssh.Dial("tcp", listener.Addr().String(), &ssh.ClientConfig{Timeout: 10 * time.Second, HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
				offered = key
				return nil
			}})

			if offered == nil || ssh.FingerprintSHA256(offered) != ssh.FingerprintSHA256(signer.PublicKey()) {
				t.Errorf("offered %v, want %s", offered, ssh.FingerprintSHA256(signer.PublicKey()))
			}
		})
	}
}

// TestCreateAtOnce checks projects whose checkouts would overlap can't be
// created at the same time.
func TestCreateAtOnce(t *testing.T) {
	for range 10 {
		srv, err := New(Config{DataDirectory: t.TempDir()}, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}

		done := make(chan error)
		for _, name := range []string{"team", "team/web"} {
			go func() { done <- srv.createProject(t.Context(), name) }()
		}

		if first, second := <-done, <-done; (first == nil) == (second == nil) {
			t.Fatalf("creating both = %v, %v, want one to fail", first, second)
		}
		if srv.exists("team") && srv.exists("team/web") {
			t.Fatal("both projects exist")
		}
	}
}

// TestHookSocket checks hooks answer only the pushes being received.
func TestHookSocket(t *testing.T) {
	ts := start(t)

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(ts.srv.runtimeDir(), "hook.sock"))
		},
	}}

	res, err := client.Post("http://pcompose/deadbeef/post-receive", "text/plain", strings.NewReader("a b refs/heads/main\n"))
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()

	if res.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown push's hook = %d, want 404", res.StatusCode)
	}
}
