package docker

import (
	"os"
	"strings"
	"testing"
)

// testManifest is what the compiler writes for a pod with an init container,
// a ConfigMap it mounts and reads as env, and a Secret it reads as env.
const testManifest = `# Managed by podcd - do not edit. Generated from Git; secrets resolved on this host.
apiVersion: v1
data:
  LEVEL: debug
  nginx.conf: "server {}\n"
kind: ConfigMap
metadata:
  name: web-config
---
apiVersion: v1
data:
  password: aHVudGVyMg==
kind: Secret
metadata:
  name: web-secret
stringData:
  token: abc
---
apiVersion: v1
kind: Pod
metadata:
  annotations:
    io.podcd.networks: backend,frontend
  labels:
    io.podcd.app: web
    io.podcd.managed: "true"
    tier: edge
  name: web
spec:
  initContainers:
  - name: seed
    image: docker.io/library/busybox:1.36
    command: ["sh", "-c", "echo seeded"]
  containers:
  - name: nginx
    image: docker.io/library/nginx:alpine
    args: ["nginx", "-g", "daemon off;"]
    ports:
    - containerPort: 80
      hostPort: 8080
      name: http
    env:
    - name: PASSWORD
      valueFrom:
        secretKeyRef:
          name: web-secret
          key: password
    - name: PLAIN
      value: "yes"
    envFrom:
    - configMapRef:
        name: web-config
      prefix: CFG_
    volumeMounts:
    - name: conf
      mountPath: /etc/nginx/conf.d
      readOnly: true
    - name: data
      mountPath: /data
    - name: logs
      mountPath: /var/log/nginx
    livenessProbe:
      httpGet:
        path: /healthz
        port: http
      periodSeconds: 5
      failureThreshold: 2
    resources:
      limits:
        memory: 64Mi
        cpu: 500m
    securityContext:
      runAsUser: 101
  restartPolicy: OnFailure
  securityContext:
    runAsGroup: 102
  volumes:
  - name: conf
    configMap:
      name: web-config
      items:
      - key: nginx.conf
        path: default.conf
  - name: data
    persistentVolumeClaim:
      claimName: web-data
  - name: logs
    emptyDir: {}
`

func TestComposeTranslatesThePod(t *testing.T) {
	m, err := parseManifest([]byte(testManifest))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.configMaps) != 1 || len(m.secrets) != 1 || m.pod.Name != "web" {
		t.Fatalf("manifest misread: %+v", m)
	}
	cf, files, err := compose("web", m, "/state/docker/web", "pause:1")
	if err != nil {
		t.Fatal(err)
	}
	if cf.Name != "podcd-web" {
		t.Fatalf("project name: %q", cf.Name)
	}

	infra := cf.Services["infra"]
	if infra == nil || infra.Image != "pause:1" || infra.ContainerName != "web-infra" || infra.Hostname != "web" {
		t.Fatalf("infra service: %+v", infra)
	}
	if infra.Labels[labelInfra] != "true" || infra.Labels["io.podcd.app"] != "web" || infra.Labels["tier"] != "edge" {
		t.Fatalf("infra labels: %v", infra.Labels)
	}
	if len(infra.Ports) != 1 || infra.Ports[0] != (port{Target: 80, Published: "8080", Protocol: "tcp"}) {
		t.Fatalf("the pod's ports are published on infra: %+v", infra.Ports)
	}
	if len(infra.Networks) != 2 || infra.Networks["backend"].Aliases[0] != "web" || !cf.Networks["frontend"].External {
		t.Fatalf("networks from the annotation, external, pod name as alias: %+v %+v", infra.Networks, cf.Networks)
	}

	seed := cf.Services["seed"]
	if seed == nil || seed.Restart != "no" || seed.NetworkMode != "service:infra" || seed.ContainerName != "web-seed" {
		t.Fatalf("init service: %+v", seed)
	}
	if strings.Join(seed.Entrypoint, " ") != "sh -c echo seeded" || seed.DependsOn["infra"].Condition != "service_started" {
		t.Fatalf("init service command/deps: %+v", seed)
	}

	nginx := cf.Services["nginx"]
	if nginx == nil || nginx.Restart != "on-failure" || nginx.NetworkMode != "service:infra" || len(nginx.Ports) != 0 {
		t.Fatalf("workload service: %+v", nginx)
	}
	if nginx.DependsOn["seed"].Condition != "service_completed_successfully" {
		t.Fatalf("the workload waits for the init container: %+v", nginx.DependsOn)
	}
	if strings.Join(nginx.Command, " ") != "nginx -g daemon off;" || nginx.Entrypoint != nil {
		t.Fatalf("args become command: %+v", nginx)
	}
	want := map[string]string{"PASSWORD": "hunter2", "PLAIN": "yes", "CFG_LEVEL": "debug", "CFG_nginx.conf": "server {}\n"}
	for k, v := range want {
		if nginx.Environment[k] != v {
			t.Fatalf("env %s: want %q, got %q (all: %v)", k, v, nginx.Environment[k], nginx.Environment)
		}
	}
	if got := strings.Join(nginx.Volumes, " "); got != "/state/docker/web/configmaps/conf:/etc/nginx/conf.d:ro data:/data logs:/var/log/nginx" {
		t.Fatalf("volumes: %q", got)
	}
	if cf.Volumes["data"].Name != "web-data" || cf.Volumes["logs"].Name != "" {
		t.Fatalf("named volumes: %+v", cf.Volumes)
	}
	if nginx.Healthcheck == nil || nginx.Healthcheck.Test[0] != "CMD-SHELL" ||
		!strings.Contains(nginx.Healthcheck.Test[1], "http://localhost:80/healthz") ||
		nginx.Healthcheck.Interval != "5s" || nginx.Healthcheck.Retries != 2 {
		t.Fatalf("healthcheck: %+v", nginx.Healthcheck)
	}
	if nginx.User != "101:102" || nginx.Deploy.Resources.Limits["memory"] != "67108864" || nginx.Deploy.Resources.Limits["cpus"] != "0.5" {
		t.Fatalf("user and limits: %+v %+v", nginx.User, nginx.Deploy)
	}

	if len(files) != 1 || files[0].path != "/state/docker/web/configmaps/conf/default.conf" ||
		string(files[0].data) != "server {}\n" || files[0].mode != 0o644 {
		t.Fatalf("mounted ConfigMap files: %+v", files)
	}

	// Same input, same bytes: the compose file must be deterministic.
	a, err := marshalCompose(cf)
	if err != nil {
		t.Fatal(err)
	}
	cf2, _, _ := compose("web", m, "/state/docker/web", "pause:1")
	b, _ := marshalCompose(cf2)
	if string(a) != string(b) {
		t.Fatal("compose output is not deterministic")
	}
	if !strings.Contains(string(a), "PASSWORD: hunter2") || !strings.Contains(string(a), "network_mode: service:infra") {
		t.Fatalf("unexpected compose file:\n%s", a)
	}
}

func TestComposeRefusesWhatDockerCannotDo(t *testing.T) {
	for _, tc := range []struct{ name, spec, want string }{
		{"unsupported volume", "  volumes:\n  - name: x\n    nfs:\n      server: h\n      path: /\n", "only hostPath, emptyDir"},
		{"fieldRef env", "  containers:\n  - name: c\n    image: i\n    env:\n    - name: N\n      valueFrom:\n        fieldRef:\n          fieldPath: metadata.name\n", "only value, configMapKeyRef"},
		{"infra name taken", "  containers:\n  - name: infra\n    image: i\n", "taken by the pod's infra container"},
	} {
		spec := "apiVersion: v1\nkind: Pod\nmetadata:\n  name: p\nspec:\n" + tc.spec
		if !strings.Contains(tc.spec, "containers:") {
			spec += "  containers:\n  - name: c\n    image: i\n"
		}
		m, err := parseManifest([]byte(spec))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if _, _, err := compose("p", m, "/d", "pause"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: want an error mentioning %q, got %v", tc.name, tc.want, err)
		}
	}
}

func TestParseManifestNeedsExactlyOnePod(t *testing.T) {
	if _, err := parseManifest([]byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n")); err == nil || !strings.Contains(err.Error(), "no Pod") {
		t.Fatalf("want 'no Pod', got %v", err)
	}
	two := "apiVersion: v1\nkind: Pod\nmetadata:\n  name: a\n---\napiVersion: v1\nkind: Pod\nmetadata:\n  name: b\n"
	if _, err := parseManifest([]byte(two)); err == nil || !strings.Contains(err.Error(), "more than one Pod") {
		t.Fatalf("want 'more than one Pod', got %v", err)
	}
	if _, err := parseManifest([]byte("apiVersion: v1\nkind: Deployment\nmetadata:\n  name: a\n")); err == nil || !strings.Contains(err.Error(), "Deployment") {
		t.Fatalf("an unknown kind must be refused, got %v", err)
	}
}

func TestHostNetworkPodHasNoPortsOrHostname(t *testing.T) {
	spec := "apiVersion: v1\nkind: Pod\nmetadata:\n  name: p\nspec:\n  hostNetwork: true\n  containers:\n  - name: c\n    image: i\n    ports:\n    - containerPort: 80\n      hostPort: 80\n"
	m, err := parseManifest([]byte(spec))
	if err != nil {
		t.Fatal(err)
	}
	cf, _, err := compose("p", m, "/d", "pause")
	if err != nil {
		t.Fatal(err)
	}
	infra := cf.Services["infra"]
	if infra.NetworkMode != "host" || infra.Hostname != "" || len(infra.Ports) != 0 {
		t.Fatalf("host network infra: %+v", infra)
	}
}

func TestSecretVolumeFilesTakeTheVolumeMode(t *testing.T) {
	spec := "apiVersion: v1\nkind: Secret\nmetadata:\n  name: s\nstringData:\n  key: v\n---\napiVersion: v1\nkind: Pod\nmetadata:\n  name: p\nspec:\n  containers:\n  - name: c\n    image: i\n    volumeMounts:\n    - name: sec\n      mountPath: /run/secrets/key\n      subPath: key\n  volumes:\n  - name: sec\n    secret:\n      secretName: s\n      defaultMode: 0400\n"
	m, err := parseManifest([]byte(spec))
	if err != nil {
		t.Fatal(err)
	}
	cf, files, err := compose("p", m, "/d", "pause")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].path != "/d/secrets/sec/key" || files[0].mode != os.FileMode(0o400) || string(files[0].data) != "v" {
		t.Fatalf("secret files: %+v", files)
	}
	if got := cf.Services["c"].Volumes[0]; got != "/d/secrets/sec/key:/run/secrets/key" {
		t.Fatalf("subPath mount: %q", got)
	}
}

func TestNestedMountInsideWrittenDirectoryGetsAMountPoint(t *testing.T) {
	spec := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: html\ndata:\n  index.html: hi\n---\n" +
		"apiVersion: v1\nkind: Secret\nmetadata:\n  name: s\nstringData:\n  token: v\n---\n" +
		"apiVersion: v1\nkind: Pod\nmetadata:\n  name: p\nspec:\n  containers:\n  - name: c\n    image: i\n    volumeMounts:\n" +
		"    - name: html\n      mountPath: /srv\n      readOnly: true\n" +
		"    - name: sec\n      mountPath: /srv/secret\n" +
		"    - name: sec\n      mountPath: /srv/one/token\n      subPath: token\n" +
		"  volumes:\n  - name: html\n    configMap:\n      name: html\n  - name: sec\n    secret:\n      secretName: s\n"
	m, err := parseManifest([]byte(spec))
	if err != nil {
		t.Fatal(err)
	}
	_, files, err := compose("p", m, "/d", "pause")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range files {
		kind := "file"
		if f.dir {
			kind = "dir"
		}
		got = append(got, kind+" "+f.path)
	}
	want := "file /d/configmaps/html/index.html, file /d/secrets/sec/token, dir /d/configmaps/html/secret, file /d/configmaps/html/one/token"
	if strings.Join(got, ", ") != want {
		t.Fatalf("want %s\ngot  %s", want, strings.Join(got, ", "))
	}
}
