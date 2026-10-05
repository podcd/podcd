---
id: contribution
title: Contributing
---

## Building and testing

```bash
make build            # compile the binary into dist/
make test             # unit tests; also runs podman's Quadlet generator over rendered units
make test-e2e         # real podman, Quadlet, systemd and Vault on this machine (starts containers)
make test-e2e-docker  # docker runtime; uses the local daemon, or starts one in a privileged container
make lint             # golangci-lint
```

`make test` needs no containers and no network. `make test-e2e` needs Linux and a user with no other podcd workloads: the suite prunes what its hosts do not declare, and refuses to start otherwise. On macOS, `contrib/macos-linux-tests.sh test-e2e` runs it inside the podman machine; the Linux-only parts of `make test` skip on a Mac, so run that through the script too.

## Pull requests

The PR title is a [Conventional Commit](https://www.conventionalcommits.org/) line (`feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `chore:`), with `!` or a `BREAKING CHANGE:` footer if it breaks a config file or a repository layout. It becomes the squash commit and drives the release notes and version bump. The [PR template](https://github.com/podcd/podcd/blob/main/.github/PULL_REQUEST_TEMPLATE.md) has the checklist.
