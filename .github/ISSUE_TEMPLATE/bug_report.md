---
name: Bug report
about: Something podcd did, or refused to do, that it should not have
title: ''
labels: bug
assignees: ''
---

<!--- Provide a general summary of the issue in the Title above -->

## Expected Behavior
<!--- Tell us what should happen -->

## Current Behavior
<!--- Tell us what happens instead of the expected behavior -->

## Possible Solution
<!--- Not obligatory, but suggest a fix/reason for the bug -->

## Steps to Reproduce (for bugs)
<!--- Provide a link to a live example, or an unambiguous set of steps to -->
<!--- reproduce this bug. Include the documents and config, with secrets and -->
<!--- private hostnames replaced. -->
1.
2.
3.

```yaml
# the Application / Pod / Host documents involved, minimised
```

```yaml
# agent.yaml (podcd config view), tokens and addresses redacted
```

## Context
<!--- How has this issue affected you? What are you trying to accomplish? -->

## Your Environment
<!--- Include as many relevant details about the environment you experienced the bug in -->
* podcd version (`podcd version`):
* Distribution and version:
* `podman --version`:
* `systemctl --user --version | head -1`:
* Output of `podcd status`, `podcd plan` and `journalctl --user -u podcd-agent -n 50`:

```text

```
