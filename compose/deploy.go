package compose

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/antoniomika/pcompose/run"
)

const (
	labelProject    = "com.docker.compose.project"
	labelService    = "com.docker.compose.service"
	labelWorkingDir = "com.docker.compose.project.working_dir"
)

// DefaultRestartPolicy is applied to deployed services that don't set their
// own, so they come back when the docker engine restarts.
const DefaultRestartPolicy = "unless-stopped"

// ElsewhereError is why a compose project isn't changed: it runs from
// somewhere other than the pcompose project's checkout.
type ElsewhereError struct {
	project, name, dir string
}

func (e *ElsewhereError) Error() string {
	return fmt.Sprintf("the compose project %s already runs from %s. Set another COMPOSE_PROJECT_NAME in %s's .env", e.name, e.dir, e.project)
}

// Deployment is a project's checkout.
type Deployment struct {
	// Name is the pcompose project.
	Name    string
	WorkDir string
	// files are the compose files, relative to WorkDir, unless fromEnv is set
	// and the checkout's .env picks them with COMPOSE_FILE.
	files   []string
	fromEnv bool
	// env is the environment compose runs with.
	env []string
}

// New describes the project checked out in workDir.
func New(name, workDir string) *Deployment {
	d := &Deployment{Name: name, WorkDir: workDir, env: Env(name, workDir)}

	if d.fromEnv = dotEnvSets(workDir, "COMPOSE_FILE"); !d.fromEnv {
		d.files = Files(workDir)
	}

	return d
}

// HasFiles reports whether there's anything to deploy.
func (d *Deployment) HasFiles() bool { return d.fromEnv || len(d.files) > 0 }

// Env is the environment a project's compose and shells run with. A .env's
// variables are left to compose.
func Env(name, workDir string) []string {
	if !strings.Contains(name, "/") || dotEnvSets(workDir, "COMPOSE_PROJECT_NAME") {
		return nil
	}

	return []string{"COMPOSE_PROJECT_NAME=" + normalizeName(strings.ReplaceAll(name, "/", "_"))}
}

// normalizeName makes name one compose accepts, as compose does for a
// project's directory.
func normalizeName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r == '-' || r == '_' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, strings.ToLower(name))

	return strings.TrimLeft(name, "_-")
}

// cmd is a compose command for the checkout's files.
func (d *Deployment) cmd(args ...string) []string {
	cmd := []string{"docker", "compose"}
	for _, f := range d.files {
		cmd = append(cmd, "-f", f)
	}

	return append(cmd, args...)
}

func (d *Deployment) run(s run.Session, args ...string) error {
	cmd := d.cmd(append([]string{"--progress=plain"}, args...)...)
	return run.Exec(s, run.Opts{Dir: d.WorkDir, Cmd: cmd, Env: d.env})
}

func (d *Deployment) output(ctx context.Context, args ...string) (string, error) {
	return run.Output(ctx, run.Opts{Dir: d.WorkDir, Cmd: d.cmd(args...), Env: d.env})
}

func (d *Deployment) config(ctx context.Context) (*project, error) {
	config, err := d.output(ctx, "config", "--format", "json")
	if err != nil {
		return nil, fmt.Errorf("read the compose files: %w", err)
	}

	return parseProject(config)
}

// checkOwner refuses to change a compose project that runs from somewhere
// else. The engine may run projects pcompose didn't deploy, pcompose's own
// among them, and compose tells projects apart only by name.
func (d *Deployment) checkOwner(ctx context.Context, name string) error {
	out, err := docker(ctx, "ps", "-a", "--filter", "label="+labelProject+"="+name, "--format", `{{.Label "`+labelWorkingDir+`"}}`)
	if err != nil {
		return err
	}

	for dir := range strings.Lines(out) {
		if dir = strings.TrimSpace(dir); dir != "" && !d.contains(dir) {
			return &ElsewhereError{project: d.Name, name: name, dir: dir}
		}
	}

	return nil
}

// contains reports whether dir is in the checkout. Compose runs a project from
// its first file's directory, which COMPOSE_FILE can put below the checkout.
func (d *Deployment) contains(dir string) bool {
	root := d.WorkDir
	if dir != root {
		root, dir = resolve(root), resolve(dir)
	}

	return dir == root || strings.HasPrefix(dir, root+string(filepath.Separator))
}

// resolve follows dir's symlinks, if it exists.
func resolve(dir string) string {
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return resolved
	}

	return dir
}

// Deploy runs docker compose up for the project. It calls named with the
// compose project's name first.
//
// pcompose changes nothing compose hashes, so `docker compose up` run by hand
// agrees with its deploys about which containers are current.
func (d *Deployment) Deploy(s run.Session, named func(name string) error) error {
	p, err := d.config(s.Context())
	if err != nil {
		return err
	}

	if err := d.checkOwner(s.Context(), p.Name); err != nil {
		return err
	}

	if err := named(p.Name); err != nil {
		return err
	}

	// Containers a failed deploy started still get the default.
	defer setDefaultRestart(s, p)

	return d.run(s, "up", "-d", "--build", "--remove-orphans")
}

// setDefaultRestart sets DefaultRestartPolicy on the containers of services
// without a restart policy. Updating a container's restart policy leaves its
// compose labels alone, so compose doesn't recreate it for this.
func setDefaultRestart(s run.Session, p *project) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.Context()), time.Minute)
	defer cancel()

	for _, name := range p.defaultRestart() {
		out, err := docker(ctx, "ps", "-aq", "--filter", "label="+labelProject+"="+p.Name, "--filter", "label="+labelService+"="+name, "--filter", "label=com.docker.compose.oneoff=False")
		if ids := strings.Fields(out); err == nil && len(ids) > 0 {
			_, err = docker(ctx, append([]string{"update", "--restart", DefaultRestartPolicy}, ids...)...)
		}

		if err != nil {
			slog.Error("Error setting the default restart policy", "project", p.Name, "service", name, "err", err)
		}
	}
}

// deployed returns the compose projects that run from the checkout, by name.
func (d *Deployment) deployed(ctx context.Context) (map[string]bool, error) {
	out, err := docker(ctx, "ps", "-a", "--filter", "label="+labelWorkingDir, "--format", `{{.Label "`+labelProject+`"}}`+"\t"+`{{.Label "`+labelWorkingDir+`"}}`)
	if err != nil {
		return nil, err
	}

	names := map[string]bool{}
	for line := range strings.Lines(out) {
		name, dir, _ := strings.Cut(strings.TrimSpace(line), "\t")
		if d.contains(dir) {
			names[name] = true
		}
	}

	return names, nil
}

// Remove removes everything deployed from the checkout, and its volumes. What
// was deployed is found by its labels and from recorded, the compose projects
// it's been deployed as, since its files may have changed or become invalid.
// Compose projects that others deployed too are left alone.
func (d *Deployment) Remove(s run.Session, recorded []string, others map[string]bool) error {
	ctx := s.Context()

	names, err := d.deployed(ctx)
	if err != nil {
		return err
	}

	// Volumes outlive their containers.
	for _, name := range recorded {
		names[name] = true
	}

	for _, name := range slices.Sorted(maps.Keys(names)) {
		if others[name] {
			_, _ = fmt.Fprintf(run.Stderr(s), "Leaving the compose project %s, which another project was deployed as too.\n", name)
			continue
		}

		var elsewhere *ElsewhereError
		if err := d.checkOwner(ctx, name); errors.As(err, &elsewhere) {
			_, _ = fmt.Fprintf(run.Stderr(s), "Leaving the compose project %s, which also runs from %s.\n", name, elsewhere.dir)
			continue
		} else if err != nil {
			return err
		}

		// From /, where there are no compose files to read instead.
		cmd := []string{"docker", "compose", "--progress=plain", "-p", name, "down", "--remove-orphans", "--volumes"}
		if err := run.Exec(s, run.Opts{Dir: "/", Cmd: cmd}); err != nil {
			return err
		}
	}

	return nil
}

// FlagError is why a compose command isn't run: it starts with a flag, which
// could name another project's files or containers after its owner is checked.
type FlagError struct{ flag string }

func (e *FlagError) Error() string {
	return fmt.Sprintf("compose's own flags, such as %s, aren't supported here. Run docker compose in the project's shell", e.flag)
}

// Compose runs a docker compose command in the project.
func (d *Deployment) Compose(s run.Session, args ...string) error {
	// Compose only takes them before its command.
	if len(args) > 0 && strings.HasPrefix(args[0], "-") {
		return &FlagError{flag: args[0]}
	}

	ctx := s.Context()
	cmd := d.cmd(args...)

	if d.HasFiles() {
		p, err := d.config(ctx)
		if err != nil {
			return err
		}

		if err := d.checkOwner(ctx, p.Name); err != nil {
			return err
		}
	} else {
		// Without files, compose finds containers by their project's name.
		names, err := d.deployed(ctx)
		if err != nil {
			return err
		}

		name := normalizeName(filepath.Base(d.WorkDir))
		if sorted := slices.Sorted(maps.Keys(names)); len(sorted) > 0 {
			name = sorted[0]
		}

		if err := d.checkOwner(ctx, name); err != nil {
			return err
		}

		cmd = append([]string{"docker", "compose", "-p", name}, args...)
	}

	return run.Exec(s, run.Opts{Dir: d.WorkDir, Cmd: cmd, Env: d.env, Stdin: true, TTY: true})
}

// EnsureNetwork creates the network the ingress reaches apps on, if it's
// missing.
func EnsureNetwork(ctx context.Context, name string) error {
	if _, err := docker(ctx, "network", "inspect", name); err == nil {
		return nil
	}

	if _, err := docker(ctx, "network", "create", name); err != nil {
		return fmt.Errorf("create the ingress network: %w", err)
	}

	return nil
}

// HasContainer reports whether the engine has a container named name.
func HasContainer(ctx context.Context, name string) bool {
	if name == "" || strings.HasPrefix(name, "-") {
		return false
	}

	_, err := docker(ctx, "container", "inspect", "--format", "{{.Id}}", name)
	return err == nil
}

func docker(ctx context.Context, args ...string) (string, error) {
	return run.Output(ctx, run.Opts{Cmd: append([]string{"docker"}, args...)})
}
