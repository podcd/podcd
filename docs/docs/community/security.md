---
id: security
title: Security
---

podcd is designed to keep the host-side trust boundary simple and explicit. The agent reads its config pointing to its Git repositories, resolves secrets locally, and writes generated unit files and runtime state in a controlled location.

- **`agent.yaml`** on the host decides which repositories to trust and which `Host` document this machine is. It is the one thing outside Git which is why it is kept minimal.
- **The repositories it names.** Whoever can merge to the pinned `revision` can change what runs on the host. Pin a tag or a commit to take that out of the loop entirely.
- Secrets are [references in Git](../configuration/secrets.md); values are resolved on the host, written to a 0600 file outside the unit directory, and appear as a hash in `podcd plan`.

The project expects deterministic behaviour and clear failures when configuration is ambiguous or invalid - the same commit always compiles to the same units, and anything the compiler is unsure about is an error.

## Reporting a vulnerability

Please report security issues privately through [GitHub's private vulnerability reporting](https://github.com/podcd/podcd/security/advisories/new).
