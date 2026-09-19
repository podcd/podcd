---
id: cli
title: CLI reference
---

Every command takes `-c`/`--config` to point at an `agent.yaml` other than the default, and most take `-o json` for a scraper. `podcd <command> --help` has the full flag list.

## Everyday

```bash
podcd status      # what is running here and when it last reconciled
podcd plan        # show changes without making them
podcd reconcile   # apply the current Git desired state
podcd health      # report application health; non-zero exit if anything is unhealthy
podcd logs api    # recent output for one application (--tail N)
```

`status` and `health` work offline: they read `state.json` and the runtime, not Git. `plan` and `reconcile` fetch first.

## The repository

```bash
podcd validate    # fetch, compile the configuration for this host, and check for errors
podcd lint        # check repository files, for every host they define, without fetching
podcd get         # what a repository defines (agent.yaml's, or --repo NAME|DIR|URL)
podcd init        # scaffold a minimal repository: one host, one nginx
podcd create      # prints a document from a kind: pod, network, host, group, environment
```

`lint` and `get` need no agent configuration when given a path - they are the two commands meant to run on a laptop against a checkout:

```bash
podcd lint                                  # the current directory
podcd lint apps.yaml hosts.yaml
podcd lint --host prod-web-01 ./gitops      # only compile one host
podcd lint --values values/common.yaml .    # the agent.yaml fallback layer for *.tpl templates

podcd get                                   # every document, with its file and line
podcd get hosts                             # NAME  ENVIRONMENT  GROUPS  APPLICATIONS  SOURCE
podcd get networks                          # NAME  DRIVER  SUBNET  SOURCE
podcd get application api -o yaml           # one document, as written
podcd get all --repo ./gitops               # a checkout you are editing
podcd get pods --repo https://github.com/podcd/podcd-gitops.git
```

## The agent

```bash
podcd run         # reconcile in a loop; what the systemd service runs
podcd install     # write the systemd user service file for the agent
podcd install --config /srv/podcd/agent.yaml   # ... running `podcd run --config /srv/podcd/agent.yaml`
podcd uninstall   # stop the agent's systemd user service and remove its unit file
podcd config      # view, create or edit agent.yaml
```

```bash
podcd config create --host prod-web-01 --repo-url git@github.com:you/gitops.git --revision main
podcd config create --path /srv/podcd/agent.yaml --env-file /srv/podcd/secrets.env --repo-url ...   # elsewhere
podcd config view
podcd config set revision v1.4.0            # alias for repository.revision
podcd config set repository.values.0 values/common.yaml
```

## Taking things down

```bash
podcd prune       # remove what podcd manages here but Git no longer declares; nothing else
podcd remove      # stop and remove applications directly, without consulting Git (--all, or by name); alias rm
podcd teardown    # remove --all, then uninstall; --purge-state and --purge-config to also delete local state/config
```

Both say what they are about to remove and ask first; `-y` skips the question.

## Shell completion

```bash
# bash
podcd completion bash | sudo tee /etc/bash_completion.d/podcd > /dev/null
# zsh
mkdir -p ~/.zsh/completions && podcd completion zsh > ~/.zsh/completions/_podcd   # then add ~/.zsh/completions to fpath before compinit
```

fish and PowerShell: `podcd completion --help`.
