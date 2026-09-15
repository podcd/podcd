---
id: contribution
title: Contributing
---

Contributions are welcome. Keep the scope of a change focused, and validate the relevant tests before opening a pull request.

## Building and testing

```bash
make build          # compile the binary into dist/
make test           # unit tests; also runs podman's Quadlet generator over rendered units
make test-e2e       # real podman, quadlet and systemd on this machine (starts containers)
make lint           # golangci-lint
```

`make test` needs no containers and no network. `make test-e2e` is the real thing - rootless podman, Quadlet and systemd on your machine; it writes unit files into `~/.config/containers/systemd` and cleans up after itself.

## Pull requests

The PR title is a [Conventional Commit](https://www.conventionalcommits.org/) line - `feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `chore:` - with `!` or a `BREAKING CHANGE:` footer if it breaks a config file or a repository layout. It becomes the squash commit and drives the release notes and the version bump; releases are cut automatically from `main`.

The pull request template asks for:

- tests for the change, with `make test` passing
- `make test-e2e` if the change touches the runtime, the renderer or the systemd units
- the README, the generated `agent.yaml` (`podcd config create`) and the examples updated if the change is visible to a user
- a check that nothing writes a secret value to Git, a unit file, a log line or `state.json`
- `make lint` and `gofmt -l .` clean
