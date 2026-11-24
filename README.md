# pcompose

Push to deploy for your own docker engine, using git, SSH, and docker compose.

```bash
git remote add pcompose ssh://example.com:2222/web.git
git push pcompose main
curl https://web.example.com
```

pcompose is for one person on one machine: an SSH server that runs your
projects directly on a docker engine you give it.

> [!NOTE]
> Want this without running a server? [picd](https://cd.pico.sh) is
> [pico.sh](https://pico.sh)'s managed service that grew out of pcompose. It's
> in closed beta for pico.sh users, and adds an isolated environment for every
> user, managed variables, deploys from GitHub and other forges, zero-downtime
> deploys, shells that resume, agent forwarding, project sharing, custom domains,
> and public IPv6. Most compose files deploy on both once you change their
> hostnames.

## Features

- Deploy with `git push`. The push shows the deploy's output, and fails if the
  deploy does.
- HTTP routing and HTTPS certificates with Traefik labels.
- Logs, and shells in your projects and containers, over SSH.

## Security

Every key in the keys directory belongs to you. Whoever has one controls the
docker engine, which on most hosts is the same as root. There are no other
users and nothing isolates projects from each other or from the host. If you
need that, use [picd](https://cd.pico.sh).

## Install

You need a host with docker and docker compose, and DNS pointing at it.

```bash
git clone https://github.com/antoniomika/pcompose && cd pcompose/deploy
cp .env.example .env # and edit it
cp ~/.ssh/id_ed25519.pub pubkeys/me
docker compose up -d
```

This runs pcompose on port 2222, and Traefik on ports 80 and 443. Files in
`pubkeys` hold public keys in the `authorized_keys` format. Changes to them
apply at the next login. Keys with options, such as `command=`, are ignored.

`PCOMPOSE_DATA` is mounted at the same path on the host and in the container.
Docker resolves a project's relative bind mounts on the host, so the paths have
to match.

pcompose runs the docker CLI, so it works with any engine that `DOCKER_HOST` or
a docker context picks. It can also run outside of a container, where its
shells are your user's:

```bash
go install github.com/antoniomika/pcompose@latest
pcompose --authentication-keys-directory ~/.config/pcompose/pubkeys \
  --private-keys-directory ~/.config/pcompose/keys \
  --data-directory ~/.local/share/pcompose
```

## Deploy a project

A project is a git repository with a compose file:

```yaml
services:
  web:
    image: traefik/whoami
    labels:
      - traefik.enable=true
      - traefik.http.routers.web.rule=Host(`web.example.com`)
    networks: [picd-ingress]
networks:
  picd-ingress:
    external: true
```

```bash
git init -b main && git add . && git commit -m init
git remote add pcompose ssh://example.com:2222/web.git
git push pcompose main
```

The first push creates the project. pcompose deploys the first branch you push.
Pushes to other branches are stored without deploying, as are branches without
a compose file. To deploy another branch:

```bash
ssh -p 2222 example.com git -C ../repos/web.git symbolic-ref HEAD refs/heads/staging
ssh -p 2222 example.com redeploy web
```

A push is refused if you changed files in the project's checkout by hand.
Untracked files, such as an app's data in `./data`, don't count, and deploys
leave them alone unless a push commits the same path.

Once a deploy starts it finishes, even if you disconnect.

### Settings and secrets

Add `.env` to the project's `.gitignore`, and put one in its checkout on the
server. Compose reads it on every deploy:

```bash
ssh -p 2222 example.com create web # if it hasn't been pushed yet
ssh -p 2222 web@example.com tee .env < .env
ssh -p 2222 example.com redeploy web # if it has
```

Or edit it in a shell with `ssh -p 2222 web@example.com`. Keep it out of
your commits: pushing one that has a `.env` replaces the server's, and a later
commit that removes it deletes it.

### Compose files

pcompose deploys the first of `compose.yaml`, `compose.yml`,
`docker-compose.yaml`, and `docker-compose.yml`, then the first matching
`.override` file. On top of those it adds every one of these that exists, for
settings that only apply on pcompose: `compose.pcompose.yaml`,
`compose.pcompose.yml`, `docker-compose.pcompose.yaml`,
`docker-compose.pcompose.yml`, `pcompose.yaml`, and `pcompose.yml`.

`COMPOSE_FILE` in the project's `.env` replaces all of this.

Services without a `restart` or `deploy.restart_policy` get `unless-stopped`.
Set `restart: "no"` on ones that run once, such as migrations, so they aren't
started again.

A compose project is named after the pcompose project, or by
`COMPOSE_PROJECT_NAME` in its `.env` or `name` in its compose file. pcompose refuses to deploy or take
down a compose project that runs from another directory, so it can't replace
something you started yourself.

## Commands

Run these with `ssh -p 2222 example.com <command>`.

| Command | |
|---|---|
| `projects` | List your projects |
| `create <project>` | Create a project before its first push |
| `redeploy [project] [--force]` | Deploy a project again without a push. `--force` discards changes made by hand |
| `logs [project] [service...] [-f]` | Show a project's logs; `-f` follows them |
| `compose [project] <args...>` | Run `docker compose` in a project, such as `ps` or `down` |
| `delete [project] --confirm <project>` | Delete a project and everything it holds, for good: its apps, volumes, repository, checkout, and `.env` |
| `help` | Show the commands |

`[project]` defaults to the SSH user, so `ssh web@example.com logs` is
`logs web`.

`compose` takes no flags before its command, such as `-p`, since they can pick
another project. Run `docker compose` in the project's shell for those.

## Shells

Anything that isn't a command above runs as a command. The SSH user picks
where:

```bash
ssh -p 2222 example.com              # a shell in your projects directory
ssh -p 2222 web@example.com          # a shell in a project
ssh -p 2222 web@example.com ls       # a command in a project
ssh -p 2222 c-web-web-1@example.com  # a shell in a container
ssh -p 2222 l-web-web-1@example.com  # follow a container's logs
ssh -p 2222 a-web-web-1@example.com  # attach to a container
```

Commands aren't run through a shell. `ssh web@example.com 'cat > .env'` doesn't
write a file, and `tee .env` or `sh -c` does.

Your local docker CLI can use the engine through pcompose:

```bash
docker -H ssh://example.com:2222 ps
```

## Using another reverse proxy

Remove Traefik from `deploy/docker-compose.yml` and attach your own proxy to the
`picd-ingress` network, or name another network with `--ingress-network`.

## Upgrading from 0.3

Projects moved from `<data>/user/app` and `<data>/user/app/app` to
`<data>/repos/user/app.git` and `<data>/projects/user/app`, and pcompose no
longer connects nginx-proxy to every project's network. For each project, with
pcompose stopped:

```bash
cd <data>
docker compose -p user_app down # keeps volumes
mkdir -p projects/user repos/user
mv user/app/app projects/user/app
mv user/app repos/user/app.git
rm -f repos/user/app.git/hooks/pre-receive repos/user/app.git/hooks/update repos/user/app.git/hooks/post-receive
```

Then remove `<data>/hooks.yml`, start the new pcompose, and push each project
again to the same URL. Add Traefik labels, or
attach your nginx-proxy and the project's services to `picd-ingress`.

Public key authentication is now always required. The `--authentication`,
password, IP and country filter, `--log-to-file`, `--cleanup-unbound`, and
container name flags are gone. To keep a host key, leave it in the keys
directory, and set `--private-key-passphrase` if it's encrypted, since the
passphrase no longer has a default.

## Configuration

Every flag can also be set in `config.yml`, or in the environment as
`PCOMPOSE_` and its name, such as `PCOMPOSE_SSH_ADDRESS`.

```text
  -k, --authentication-keys-directory string   Directory of the public keys that may log in. Changes apply at the next login (default "deploy/pubkeys")
  -c, --config string                          Config file (default "config.yml")
      --data-directory string                  Directory that holds repositories, checkouts, and pcompose's git hooks. The docker engine must see it at the same path for relative bind mounts to work (default "deploy/data")
      --debug                                  Log debugging information, which includes commands' arguments
      --deploy-timeout duration                How long a deploy may take (default 30m0s)
  -h, --help                                   help for pcompose
      --ingress-network string                 The docker network the ingress reaches apps on, created if it's missing. Empty creates none (default "picd-ingress")
  -p, --private-key-passphrase string          Passphrase for encrypted SSH host keys in the private keys directory
  -l, --private-keys-directory string          Directory of SSH host keys. pcompose generates ssh_host_ed25519_key in it (default "deploy/keys")
      --runtime-directory string               Directory that holds the git hooks and their socket, which only pcompose's user may use. Defaults to run in the data directory
      --shutdown-timeout duration              How long pushes, commands, and deploys have to finish when pcompose is asked to stop (default 5m0s)
  -a, --ssh-address string                     The address to listen for SSH connections on (default "localhost:2222")
  -v, --version                                version for pcompose
```

## Development

```bash
go test ./...
PCOMPOSE_INTEGRATION=1 go test ./... # also deploys on this machine's docker engine
```

## License

See [LICENSE](LICENSE).
