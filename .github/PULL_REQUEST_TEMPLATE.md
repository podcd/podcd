<!--- Title: a Conventional Commit line, e.g. "feat(vault): support KV v1 mounts". -->

## Description
<!--- What changed and why. Link the issue if any: Fixes #123 -->

## Testing
<!--- What you ran. If you reconciled a real host, say which distribution and podman/docker version. -->

## Checklist
- [ ] Tests written; `make test` passes.
- [ ] `make test-e2e` if the runtime, renderer or systemd units changed; `make test-e2e-docker` if the docker runtime or compose translation changed.
- [ ] Docs, `podcd config create` output and `examples/` updated if user-visible.
- [ ] No secret value reaches Git, a unit file, a log line or `state.json`.
- [ ] `make lint` and `gofmt -l .` clean.
