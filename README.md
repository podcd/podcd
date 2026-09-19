<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/logo-dark.svg">
    <img src="assets/logo.svg" alt="podcd" width="200">
  </picture>
</p>

<p align="center">A Git-driven reconciler for Linux workloads.</p>
<p align="center"><a href="https://podcd.github.io/podcd/">Documentation</a></p>

podcd reconciles a Linux host to match what is declared in Git.

- Git defines the desired state: pods, the networks they join, and the secrets they read.
- A local agent reads that state and identifies the host.
- The agent compiles the resulting configuration.
- Quadlet and systemd manage the running containers and networks.

Hosts without podman can run the same repository on Docker (`runtime: docker` in `agent.yaml`): each pod becomes a Compose project with a pause container standing in for the pod. See [Docker runtime](https://podcd.github.io/podcd/configuration/docker).

## Quick Start

Install the binary.

```bash
V=$(curl -fsSLI https://github.com/podcd/podcd/releases/latest \
  | sed -n 's/^[Ll]ocation:.*\/tag\/v\([^[:space:]]*\).*/\1/p' \
  | tr -d '\r')

A=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')

curl -fsSLO "https://github.com/podcd/podcd/releases/download/v$V/podcd_${V}_linux_${A}.tar.gz"
curl -fsSLO "https://github.com/podcd/podcd/releases/download/v$V/podcd_${V}_linux_${A}.tar.gz.sha256"

sha256sum -c "podcd_${V}_linux_${A}.tar.gz.sha256" &&
  tar -xzf "podcd_${V}_linux_${A}.tar.gz" &&
  sudo install -m 0755 podcd /usr/local/bin/podcd
```

Create config pointing `podcd` to a git repo.
```bash
podcd config create --host <this_is_optional> --repo-url <repo> --revision main
```

Use `podcd` to provision the systemd service
```bash
podcd install
```

Start the service
```bash
systemctl --user daemon-reload && systemctl --user enable --now podcd-agent.service
```

```bash
podcd logs  # opens podcd-agent.service logs
```

To setup a `podcd` gitops repository you can refer to the following documents:
* [Basic Documents](https://podcd.github.io/podcd/configuration/model)
* [Templating](https://podcd.github.io/podcd/configuration/values)
* [Secrets](https://podcd.github.io/podcd/configuration/secrets)

```text
Usage:
  podcd [flags]
  podcd [command]

Available Commands:
  completion  Generate the autocompletion script for the specified shell
  config      view or create the agent config file
  create      print a validated document for the repository
  get         list what a repository defines
  health      report what podman says about the applications this host runs
  help        Help about any command
  init        scaffold a minimal repository
  install     write the systemd user service file that runs the agent
  lint        check repository files without fetching or touching the host
  logs        recent log output for the agent, or for one application
  options     print the list of flags inherited by all commands
  plan        what would change, without changing anything
  prune       remove what podcd manages here but Git no longer declares
  reconcile   make this host match Git
  remove      stop and remove applications directly, without consulting Git
  run         reconcile in a loop (this is what the systemd service runs)
  status      what this host is running and when it last reconciled
  teardown    stop and remove everything podcd runs on this host, and uninstall the agent
  uninstall   stop the agent's systemd user service and remove its unit file
  validate    load Git and compile the configuration for a host
  version     print the version

Flags:
  -h, --help   help for podcd

Use "podcd [command] --help" for more information about a command.
```

### Shell completion

```bash
# bash
podcd completion bash | sudo tee /etc/bash_completion.d/podcd > /dev/null
# zsh
mkdir -p ~/.zsh/completions && podcd completion zsh > ~/.zsh/completions/_podcd
# then add ~/.zsh/completions to fpath before compinit
```

fish and PowerShell: `podcd completion --help`.

### Container image

```bash
make image                                              # builds podcd:$(VERSION) from Containerfile
podman run --rm -v /repo:/repo:ro,Z ghcr.io/podcd/podcd:latest lint /repo
```

The image runs as a fixed non-root UID (1000); rootless Podman remaps that into your subordinate uid/gid range the same way it does for any other container, nothing special to configure here.

On SELinux-enforcing hosts (RHEL, Fedora), add `:z` (shared) or `:Z` (private) to any bind mount as above, or the mount is denied - Debian/Ubuntu/Alpine don't need this since they don't enforce SELinux.

## Testing

```bash
make test           # unit tests; also runs podman's Quadlet generator over rendered units
make test-e2e       # real podman, quadlet, systemd and Vault on this machine (starts containers)
make test-e2e-docker # the docker runtime; uses the Docker daemon here, or starts one in a privileged container
make lint           # golangci-lint
```

`make test-e2e` needs Linux and a user with no other podcd workloads (the
suite prunes what its hosts do not declare, and refuses to start otherwise).
On macOS, `contrib/macos-linux-tests.sh test-e2e` runs it inside the podman
machine; the Linux-only parts of `make test` skip on a Mac, so run that
through the script too before relying on a green run.

## Security

Report security issues through [GitHub's private vulnerability reporting](https://github.com/podcd/podcd/security/advisories/new).

## Contributing

See [Contributing](docs/docs/community/contribution.md).

## License

MIT. See [LICENSE](LICENSE).
