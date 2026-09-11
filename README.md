# podcd

A Git-driven reconciler for containers on ordinary Linux VMs.

Git says what should run. A small agent on each VM pulls Git, works out what belongs on *this* machine, compares that with what is actually there, and performs continuous deployment. 

Applications run as rootless Podman containers supervised by systemd through Quadlet.
It is not Kubernetes.

```text
* Git desired state
* Agent  (pull -> identify host -> compile -> plan)
* Quadlet unit files
* Running app
```

`~/.local/state/podcd/state.json` records the agent's identity, the last revisions, the last successful and failed reconcile, and per-application history including the previous deployment. 
It is metadata and is safe to delete. Agent recomputes everything from Git and the host.

## Quick start

On a Debian or Ubuntu VM:

```bash
make build
sudo ./deploy/bootstrap.sh \
  --repo-url https://git.example.com/infrastructure.git \
  --user podcd \
  --host prod-web-01 \
  --binaries ./dist
```

That installs podman, creates an unprivileged `podcd` user, enables lingering so
the applications come back after a reboot, and starts the agent as a systemd
*user* service. From then on the VM reconciles itself every minute.

To see what it is doing:

```bash
podcd status      # what is running here, and when it last reconciled
podcd plan        # what would change, without changing anything
podcd reconcile   # make it so, now
podcd health      # probe the applications
podcd logs api    # recent output from one application
podcd validate    # compile the configuration and check it
```

## Configuration

Four kinds of document, all `apiVersion: gitops.podcd.io/v1`, living in one or more Git repositories:

```text
* Environment   what is true of a whole environment
* Group         what a machine is for (a role)
* Host          this VM: its environment, its groups, its own overrides
* Application   what an app is, defined once and referenced by name
```

A host:

```yaml
apiVersion: gitops.podcd.io/v1
kind: Host
metadata:
  name: prod-web-01
spec:
  environment: production
  groups: [web]
  applications: [debug-tools]     # extras, on top of the groups
  excludeApplications: [frontend] # opt out of something a group brings
  overrides:
    api:
      env:
        LOG_LEVEL: debug
```

An application:

```yaml
apiVersion: gitops.podcd.io/v1
kind: Application
metadata:
  name: api
spec:
  image: docker.io/library/nginx:latest
  ports:
    - host: 8080
      container: 8080
      hostIP: 127.0.0.1
  env:
    APP_ENV: production
  secretEnv:
    DATABASE_PASSWORD: env:API_DATABASE_PASSWORD
  restartPolicy: always
  healthcheck:
    http:
      port: 8080
      path: /health
```
### Pod manifests

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: metrics
spec:
  containers:
    - name: collector
      image: docker.io/library/nginx:latest
      envFrom:
        - configMapRef: { name: metrics-config }
      ports:
        - containerPort: 9200
          hostPort: 9200
      readinessProbe:
        httpGet: { path: /ready, port: 9200 }
```

The same rules apply as to an `Application`: every container image must be digest-pinned (or the pod carries `gitops.podcd.io/allow-mutable-image: "true"`), referenced ConfigMaps and Secrets must exist unless marked `optional`, host ports are checked for conflicts, and the first readiness (or liveness) probe on a published port becomes the health check.

A `Secret` in Git holds references, never values: `data:` is refused, and every
`stringData` value must be `env:NAME` or `file:path`. Values are resolved on the
host, written into a 0600 manifest outside the unit directory, and shown as a
hash in `podcd plan`.

Overrides for a Pod are strategic merge patches - containers merge by name,
ports by containerPort - which is what a Pod author expects:

```yaml
overrides:
  metrics:
    spec:
      containers:
        - name: shipper
          args: ["--target", "http://127.0.0.1:9200", "--verbose"]
```

### Inheritance

Lowest precedence to highest:

```text
Application -> Environment override -> Group overrides -> Host override
```

Groups are applied in the order the host lists them, so later entries win. Scalars and map entries are overridden key by key; lists (ports, volumes, command) are replaced wholesale, because appending to a port list has no sane meaning. Applications are the union of what the environment, the groups and the host ask for, minus what the host excludes, sorted by name.

Two runs of the compiler on the same commit produce the same bytes. Ambiguity is an error, never a guess: an application defined twice across repositories, a reference to an application that does not exist, an unknown group, a host with no `Host` document, a misspelled field, two applications fighting over one host port - each of those fails the reconcile with a message naming the file.

### Multiple repositories

```yaml
repositories:
  - name: infrastructure
    url: https://git.example.com/infrastructure.git
    revision: main
  - name: applications
    url: https://git.example.com/applications.git
    revision: main
    path: clusters/prod   # optional subdirectory
```

## Testing

```bash
make test           # unit tests
make test-e2e       # podman, quadlet, systemd, on local machine
```
