<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/logo-dark.svg">
    <img src="assets/logo.svg" alt="podcd" width="200">
  </picture>
</p>

<p align="center">A Git-driven reconciler for Linux workloads.</p>
<p align="center"><a href="https://podcd.github.io/podcd/">Documentation</a></p>

podcd reconciles a Linux host to match Git: pods, the networks they join, and the secrets they read. A local agent compiles the repository for its host; Quadlet and systemd run the containers. Hosts without podman can use [Docker](https://podcd.github.io/podcd/configuration/docker) instead.

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

Configure, install and start the agent:

```bash
podcd config create --repo-url <repo> --revision main   # --host defaults to the hostname
podcd install
systemctl --user daemon-reload && systemctl --user enable --now podcd-agent.service
podcd logs
```

Repository setup: [model](https://podcd.github.io/podcd/configuration/model), [templating](https://podcd.github.io/podcd/configuration/values), [secrets](https://podcd.github.io/podcd/configuration/secrets). All commands: [CLI reference](https://podcd.github.io/podcd/reference/cli).

## Security

Report security issues through [GitHub's private vulnerability reporting](https://github.com/podcd/podcd/security/advisories/new).

## Contributing

Building and testing: [Contributing](docs/docs/community/contribution.md).

## License

MIT. See [LICENSE](LICENSE).
