# pcompose

An open source PaaS using docker-compose.

## Deploy

Builds are made automatically on Dockerhub. Feel free to either use the automated binaries or to build your own.

1. Clone this repo
    - `git clone https://github.com/antoniomika/pcompose`
2. Use docker-compose to stand up pcompose and nginx-proxy with Let's Encrypt
    - `docker-compose -p pcompose -f deploy/docker-compose.yml up -d`

And you're ready to start using it!

## How it works

pcompose is ultimately a wrapper around the docker-compose and docker CLIs. I could've integrated it directly with the APIs, but there were quite a few hacks that were needed to make that happen. Also, this made it extremely easy to continue to work with the familiar and easy to use CLIs

pcompose implements an SSH server, which you can add authentication to, to allow you to push a git repo which is then used to stand up a docker-compose project. pcompose then offers some convenience features as well easy access to these deployed services, all from within a docker-compose.yml file.

Here's an example `docker-compose.yml` file that works with pcompose:

```yml
version: '3.7'
services:
  whoami:
    image: kennethreitz/httpbin
    environment:
      - VIRTUAL_HOST=http.example.com
      - LETSENCRYPT_HOST=http.example.com
      - LETSENCRYPT_EMAIL=certs@example.com
```

This configuration will deploy [httpbin](https://httpbin.org) to be accessible from the address `https://http.example.com` and will automatically setup Let's Encrypt and the nginx-proxy to the resource. All you need to do is:

1. Place that file into a directory
    - `mkdir httpbin && touch httpbin/docker-compose.yml`
2. Initialize a git repo
    - `cd httpbin && git init`
3. Add and commit the file to the new repo
    - `git add . && git commit -m "Init"`
4. Add a remote that points to pcompose
    - `git remote add origin pcompose ssh://example.com:2222/user/httpbin`
5. Push to the pcompose remote
    - `git push pcompose master`

After step 5, you should see some output that looks like this:

```text
Enumerating objects: 5, done.
Counting objects: 100% (5/5), done.
Delta compression using up to 12 threads
Compressing objects: 100% (2/2), done.
Writing objects: 100% (3/3), 668 bytes | 668.00 KiB/s, done.
Total 3 (delta 1), reused 0 (delta 0)
remote: Creating user_httpbin_whoami_1 ... done
remote:
To ssh://example.com:2222/user/httpbin
   95fae00..a34685d  master -> master
```

Once that's done, you should be ready to access your service at `https://http.example.com`

## Features

There are a few useful features that are implemented into pcompose.

### Integrated Shell

The first is a linux shell with access to docker-compose and docker CLIs which is scoped to each project:

```bash
ssh -p 2222 user/httpbin@example.com
```

Will drop you right into a shell where you can run docker-compose or docker in the remote context.

### Remote docker-compose

You can also use docker-compose through pcompose in the project context easily:

```bash
ssh -p 2222 user/httpbin@example.com ps -a
```

or

```bash
ssh -p 2222 user/httpbin@example.com down
```

### Exec into containers

Next, you can exec directly into a container through pcompose (granted, the container needs to have `/bin/sh`):

```bash
ssh -p 2222 user_httpbin_whoami_1@example.com
```

or

```bash
ssh -p 2222 c-user_httpbin_whoami_1@example.com
```

### Container logs

You can grab and follow container logs through pcompose:

```bash
ssh -p 2222 l-user_httpbin_whoami_1@example.com
```

## Caveats

### nginx-proxy

[nginx-proxy](https://github.com/nginx-proxy/nginx-proxy) is a pretty great tool to handle dynamic host configuration. This can easily be swapped for [traefik](https://containo.us/traefik/) or some other reverse proxy that supports configuration using docker.

The way this currently works is pcompose will create the default network for docker-compose and will attach `nginx-proxy` to the created network. This means you must use the default network or at least pre-create the network and attach `nginx-proxy` to it in order to use it for HTTP(S) reverse proxying

### Persistence

`docker-compose` allows the use of relative directories for defining data volumes in applications. I recommend using relative directories from your application to make it easy for you to find your data when you need to.

I also recommend to bind mount the `--data-directory` to be the same in both the host and the pcompose container. If your application uses a relative mount and the data directory is not the same, your persistence data could end up in a different location than intended!

## CLI Flags

```text
pcompose is a command line utility that runs a simple PaaS ontop of docker using docker-compose and git

Usage:
  pcompose [flags]

Flags:
      --authentication                         Require authentication for the SSH service
  -k, --authentication-keys-directory string   Directory where public keys for public key authentication are stored.
                                               pcompose will watch this directory and automatically load new keys and remove keys
                                               from the authentication list (default "deploy/pubkeys/")
  -u, --authentication-password string         Password to use for ssh server password authentication (default "S3Cr3tP4$$W0rD")
  -o, --banned-countries string                A comma separated list of banned countries. Applies to SSH connections
  -x, --banned-ips string                      A comma separated list of banned ips that are unable to access the service. Applies to SSH connections
      --cleanup-unbound                        Cleanup unbound (unforwarded) SSH connections after a set timeout (default true)
  -c, --config string                          Config file (default "config.yml")
      --data-directory string                  Directory that holds pcompose data (default "deploy/data/")
      --debug                                  Enable debugging information
      --frontend-container-name string         The name of the frontend container in order to connect it to the default docker-compose network. (default "nginx-proxy")
      --geodb                                  Use a geodb to verify country IP address association for IP filtering
  -h, --help                                   help for pcompose
      --log-to-file                            Enable writing log output to file, specified by log-to-file-path
      --log-to-file-compress                   Enable compressing log output files
      --log-to-file-max-age int                The maxium number of days to store log output in a file (default 28)
      --log-to-file-max-backups int            The maxium number of rotated logs files to keep (default 3)
      --log-to-file-max-size int               The maximum size of outputed log files in megabytes (default 500)
      --log-to-file-path string                The file to write log output to (default "/tmp/pcompose.log")
      --log-to-stdout                          Enable writing log output to stdout (default true)
      --pcompose-container-name string         The name of the pcompose container in order to exec into a context. (default "pcompose")
  -l, --private-key-location string            The location of the SSH server private key. pcompose will create a private key here if
                                               it doesn't exist using the --private-key-passphrase to encrypt it if supplied (default "deploy/keys/ssh_key")
  -p, --private-key-passphrase string          Passphrase to use to encrypt the server private key (default "S3Cr3tP4$$phrAsE")
  -a, --ssh-address string                     The address to listen for SSH connections (default "localhost:2222")
      --time-format string                     The time format to use for general log messages (default "2006/01/02 - 15:04:05")
  -v, --version                                version for pcompose
  -y, --whitelisted-countries string           A comma separated list of whitelisted countries. Applies to SSH connections
  -w, --whitelisted-ips string                 A comma separated list of whitelisted ips. Applies to SSH connections
```
