package config

import (
	"context"
	"strings"
	"testing"
)

const podFiles = `
apiVersion: v1
kind: ConfigMap
metadata:
  name: web-config
data:
  GREETING: hello
---
apiVersion: v1
kind: Pod
metadata:
  name: web
spec:
  containers:
    - name: front
      image: example.com/front` + digest + `
      envFrom:
        - configMapRef:
            name: web-config
      ports:
        - containerPort: 8080
          hostPort: 18080
      readinessProbe:
        httpGet:
          path: /health
          port: 8080
    - name: side
      image: example.com/side` + digest + `
      args: ["--mode", "a"]
---
apiVersion: gitops.podcd.io/v1
kind: Host
metadata:
  name: vm-1
spec:
  applications: [web]
`

func resolvePod(t *testing.T, files map[string]string, host string) (string, error) {
	t.Helper()
	ix := NewIndex()
	if err := ix.LoadTree("test", writeTree(t, files)); err != nil {
		return "", err
	}
	got, err := ix.Resolve(context.Background(), ResolveOptions{Host: host})
	if err != nil {
		return "", err
	}
	if len(got.Applications) != 1 {
		t.Fatalf("want one workload, got %v", got.Names())
	}
	app := got.Applications[0]
	if !app.IsKube() {
		t.Fatalf("want a kube workload, got kind %q", app.Kind)
	}
	return string(app.Manifest), nil
}

func TestPodCompilesToAManifestWithItsConfigMap(t *testing.T) {
	manifest, err := resolvePod(t, map[string]string{"pod.yaml": podFiles}, "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"kind: ConfigMap", "GREETING: hello", "kind: Pod", "name: front", "name: side",
		"io.podcd.managed: \"true\"", "io.podcd.app: web"} {
		if !strings.Contains(manifest, want) {
			t.Errorf("manifest is missing %q:\n%s", want, manifest)
		}
	}
	// The ConfigMap must come before the Pod: podman reads the stream in order.
	if strings.Index(manifest, "kind: ConfigMap") > strings.Index(manifest, "kind: Pod") {
		t.Error("ConfigMap should precede the Pod in the played manifest")
	}
}

func TestPodManifestIsDeterministic(t *testing.T) {
	first, err := resolvePod(t, map[string]string{"pod.yaml": podFiles}, "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := resolvePod(t, map[string]string{"pod.yaml": podFiles}, "vm-1")
		if err != nil {
			t.Fatal(err)
		}
		if again != first {
			t.Fatalf("manifest differs between runs:\n%s\n---\n%s", first, again)
		}
	}
}

func TestPodHealthcheckComesFromTheReadinessProbe(t *testing.T) {
	ix := loadIndex(t, map[string]string{"pod.yaml": podFiles})
	got, err := ix.Resolve(context.Background(), ResolveOptions{Host: "vm-1"})
	if err != nil {
		t.Fatal(err)
	}
	hc := got.Applications[0].Healthcheck
	if hc == nil || hc.HTTP == nil {
		t.Fatalf("want an http probe derived from the readinessProbe, got %+v", hc)
	}
	if hc.HTTP.Port != 18080 || hc.HTTP.Path != "/health" {
		t.Errorf("probe should target the host port: %+v", hc.HTTP)
	}
	ports := got.Applications[0].Ports
	if len(ports) != 1 || ports[0].Host != 18080 || ports[0].Container != 8080 {
		t.Errorf("host ports should be collected for conflict checks: %+v", ports)
	}
}

func TestPodImageMayBeATag(t *testing.T) {
	files := map[string]string{"pod.yaml": strings.Replace(podFiles, "example.com/side"+digest, "example.com/side:latest", 1)}
	if _, err := resolvePod(t, files, "vm-1"); err != nil {
		t.Fatalf("a tag is an acceptable container image: %v", err)
	}
}

func TestPodMisspelledFieldIsRejected(t *testing.T) {
	ix := NewIndex()
	err := ix.LoadTree("test", writeTree(t, map[string]string{"pod.yaml": strings.Replace(podFiles, "containers:", "containerz:", 1)}))
	if err == nil || !strings.Contains(err.Error(), "containerz") {
		t.Fatalf("a misspelled Pod field must fail loudly, got: %v", err)
	}
}

func TestPodMissingConfigMapIsAnError(t *testing.T) {
	files := map[string]string{"pod.yaml": strings.Replace(podFiles, "name: web-config\ndata", "name: other-config\ndata", 1)}
	_, err := resolvePod(t, files, "vm-1")
	if err == nil || !strings.Contains(err.Error(), `ConfigMap "web-config", which is not defined`) {
		t.Fatalf("a missing ConfigMap must be reported, got: %v", err)
	}
}

func TestPodOptionalConfigMapMayBeMissing(t *testing.T) {
	files := map[string]string{"pod.yaml": strings.Replace(
		strings.Replace(podFiles, "name: web-config\ndata", "name: other-config\ndata", 1),
		"            name: web-config\n", "            name: web-config\n            optional: true\n", 1)}
	if _, err := resolvePod(t, files, "vm-1"); err != nil {
		t.Fatalf("an optional reference may be missing: %v", err)
	}
}

func TestPodAndApplicationCannotShareAName(t *testing.T) {
	files := map[string]string{"pod.yaml": podFiles, "app.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Application
metadata:
  name: web
spec:
  image: example.com/web` + digest + `
`}
	ix := NewIndex()
	err := ix.LoadTree("test", writeTree(t, files))
	if err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("a Pod and an Application with one name is ambiguous, got: %v", err)
	}
}

func TestPodOverrideIsAStrategicMergePatch(t *testing.T) {
	files := map[string]string{"pod.yaml": strings.Replace(podFiles, "  applications: [web]\n", `  applications: [web]
  overrides:
    web:
      spec:
        containers:
          - name: side
            args: ["--mode", "b"]
`, 1)}
	manifest, err := resolvePod(t, files, "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(manifest, "- b") || strings.Contains(manifest, "- a\n") {
		t.Errorf("the sidecar's args should be patched:\n%s", manifest)
	}
	// Merging by container name means the front container is untouched.
	if !strings.Contains(manifest, "name: front") || !strings.Contains(manifest, "GREETING") {
		t.Errorf("the front container was lost by the patch:\n%s", manifest)
	}
}

func TestPodOverrideWithUnknownFieldIsRejected(t *testing.T) {
	files := map[string]string{"pod.yaml": strings.Replace(podFiles, "  applications: [web]\n", `  applications: [web]
  overrides:
    web:
      spec:
        containers:
          - name: side
            imagee: nope
`, 1)}
	_, err := resolvePod(t, files, "vm-1")
	if err == nil || !strings.Contains(err.Error(), "imagee") {
		t.Fatalf("a patch with an unknown field must be rejected, got: %v", err)
	}
}

func TestSecretInGitMustBeReferenceOnly(t *testing.T) {
	ix := NewIndex()
	err := ix.LoadTree("test", writeTree(t, map[string]string{"s.yaml": `
apiVersion: v1
kind: Secret
metadata:
  name: db
stringData:
  PASSWORD: hunter2
`}))
	if err == nil || !strings.Contains(err.Error(), "must be references") {
		t.Fatalf("a literal secret value must be refused, got: %v", err)
	}

	ix = NewIndex()
	err = ix.LoadTree("test", writeTree(t, map[string]string{"s.yaml": `
apiVersion: v1
kind: Secret
metadata:
  name: db
data:
  PASSWORD: aHVudGVyMg==
`}))
	if err == nil || !strings.Contains(err.Error(), "plaintext values in data") {
		t.Fatalf("base64 is not encryption; data must be refused, got: %v", err)
	}
}


func TestPodMissingSecretFailsTheReconcile(t *testing.T) {
	files := map[string]string{"pod.yaml": strings.Replace(podFiles, "      envFrom:\n", `      env:
        - name: PASSWORD
          valueFrom:
            secretKeyRef:
              name: db
              key: PASSWORD
      envFrom:
`, 1)}
	_, err := resolvePod(t, files, "vm-1")
	if err == nil || !strings.Contains(err.Error(), `Secret "db", which is not defined`) {
		t.Fatalf("a missing Secret must be reported, got: %v", err)
	}
}
