<!--- Title: a Conventional Commit line, e.g. "feat(vault): support KV v1 mounts". -->
<!--- It becomes the squash commit and drives the release notes and version bump. -->

## Description
<!--- Describe your changes in detail. -->

## Motivation and Context
<!--- Why is this change required? What problem does it solve? -->
<!--- If it fixes an open issue, link it here: Fixes #123 -->

## How Has This Been Tested?
<!--- Describe how you tested your changes and what you ran: -->
<!---   make test      unit tests, no containers -->
<!---   make test-e2e  rootless podman + systemd on your machine -->
<!--- If you reconciled a real host, say which distribution and podman version. -->

## Checklist
<!--- Put an `x` in every box that applies. Unsure? Ask in the PR - that is what review is for. -->
- [ ] The title is a Conventional Commit (`feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `chore:`), with `!` or a `BREAKING CHANGE:` footer if it breaks a config file or a repository layout.
- [ ] I have written tests for my code changes, and `make test` passes.
- [ ] I ran `make test-e2e` if the change touches the runtime, renderer, or systemd units.
- [ ] I have updated the README, the generated `agent.yaml` (`config create`), and the `examples/` if the change is visible to a user.
- [ ] Nothing in this change writes a secret value to Git, a unit file, a log line, or `state.json`.
- [ ] `make lint` and `gofmt -l .` are clean.
