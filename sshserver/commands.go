package sshserver

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/antoniomika/pcompose/compose"
	"github.com/picosh/pico/pkg/pssh"
)

// command is a management command, run as `ssh <host> <name> [args]`. Names
// avoid common programs, since other commands run in the projects directory.
type command struct {
	name    string
	args    string
	summary string
	// project commands act on their first argument if it names a project,
	// and otherwise on the project the session connected as.
	project bool
	run     func(c *call) error
}

// call is one run of a command.
type call struct {
	srv     *Server
	s       *pssh.SSHServerConnSession
	cmd     *command
	project string
	args    []string
}

// commands is filled in init, since help reads it.
var commands []*command

func init() {
	commands = []*command{
		{name: "projects", summary: "List your projects", run: runProjects},
		{name: "create", args: "<project>", summary: "Create a project before its first push, such as to give it a .env", run: runCreate},
		{name: "redeploy", args: "[project] [--force]", summary: "Deploy a project again without a push", project: true, run: runRedeploy},
		{name: "logs", args: "[project] [service...] [-f]", summary: "Show a project's logs; -f follows them", project: true, run: runLogs},
		{name: "compose", args: "[project] <args...>", summary: "Run docker compose in a project, such as ps or down", project: true, run: runCompose},
		{name: "delete", args: "[project] --confirm <project>", summary: "Delete a project and everything it holds, for good", project: true, run: runDelete},
		{name: "help", summary: "Show this help", run: runHelp},
	}
}

// findCommand returns the management command argv runs, and its arguments, or
// nil.
func findCommand(argv []string) (*command, []string) {
	if len(argv) > 0 {
		for _, cmd := range commands {
			if cmd.name == argv[0] {
				return cmd, argv[1:]
			}
		}
	}

	return nil, nil
}

func (cmd *command) usage() string {
	return strings.TrimSpace(cmd.name + " " + cmd.args)
}

// interactive reports whether the command runs for as long as its client wants
// it to, as compose does in a terminal, so stopping doesn't wait for it.
func (cmd *command) interactive(args []string, pty bool) bool {
	return cmd.name == "logs" || cmd.name == "compose" && (pty || slices.Contains(args, "logs"))
}

func helpText() string {
	var b strings.Builder

	row := func(left, right string) {
		_, _ = fmt.Fprintf(&b, "  %-36s %s\n", left, right)
	}

	b.WriteString("pcompose deploys the projects you push to it:\n\n")
	b.WriteString("  git remote add pcompose ssh://<host>/<project>.git\n")
	b.WriteString("  git push pcompose main\n\n")
	b.WriteString("Commands, run with ssh <host> <command>\n")

	for _, cmd := range commands {
		row(cmd.usage(), cmd.summary)
	}

	b.WriteString("\n[project] defaults to the SSH user, so ssh web@<host> logs is logs web.\n")
	b.WriteString("\nShells\n")
	row("ssh <host>", "A shell in your projects directory")
	row("ssh <project>@<host> [command]", "A shell, or a command, in a project")
	row("ssh c-<container>@<host> [command]", "A shell, or a command, in a container")
	row("ssh l-<container>@<host>", "Follow a container's logs")
	row("ssh a-<container>@<host>", "Attach to a container")

	return b.String()
}

// runCommand runs a management command, resolving its project first.
func (srv *Server) runCommand(s *pssh.SSHServerConnSession, cmd *command, args []string) error {
	c := &call{srv: srv, s: s, cmd: cmd, args: args}

	if !cmd.project {
		return cmd.run(c)
	}

	switch {
	case len(args) > 0 && srv.exists(projectName(args[0])):
		c.project, c.args = projectName(args[0]), args[1:]
	case srv.exists(s.User()):
		c.project = s.User()
	case len(args) > 0:
		return usagef("no project named %s. Run 'projects' to list yours", args[0])
	default:
		return usagef("name a project: %s", cmd.usage())
	}

	return cmd.run(c)
}

func (c *call) usageError() error {
	return usagef("usage: %s", c.cmd.usage())
}

func (c *call) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(c.s, format, args...)
}

func (c *call) deployment() *compose.Deployment {
	return compose.New(c.project, c.srv.WorktreePath(c.project))
}

func runHelp(c *call) error {
	_, err := io.WriteString(c.s, helpText())
	return err
}

func runProjects(c *call) error {
	projects, err := c.srv.projects()
	if err != nil {
		return err
	}

	if len(projects) == 0 {
		c.printf("No projects yet. Push one with git push.\n")
	}

	for _, name := range projects {
		c.printf("%s\n", name)
	}

	return nil
}

func runCreate(c *call) error {
	if len(c.args) != 1 {
		return c.usageError()
	}

	name := projectName(c.args[0])
	if c.srv.exists(name) {
		return usagef("%s already exists", name)
	}

	if err := c.srv.createProject(c.s.Context(), name); err != nil {
		return err
	}

	c.printf("Created %s. Push to it to deploy.\n", name)
	return nil
}

func runRedeploy(c *call) error {
	force := false

	for _, arg := range c.args {
		if arg != "--force" && arg != "-f" {
			return c.usageError()
		}
		force = true
	}

	return c.srv.deploy(c.s, c.project, force)
}

func runLogs(c *call) error {
	return c.deployment().Compose(c.s, append([]string{"logs", "--tail=100"}, c.args...)...)
}

func runCompose(c *call) error {
	if len(c.args) == 0 {
		return c.usageError()
	}

	return c.deployment().Compose(c.s, c.args...)
}

func runDelete(c *call) error {
	name := c.project

	if len(c.args) != 2 || c.args[0] != "--confirm" || c.args[1] != name {
		return usagef("deleting %s removes its apps, volumes, repository, and checkout, with its .env, for good. Run 'delete %s --confirm %s' to go ahead", name, name, name)
	}

	if err := c.srv.delete(c.s, name); err != nil {
		return err
	}

	c.printf("Deleted %s.\n", name)
	return nil
}
