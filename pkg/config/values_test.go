package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMergeValuesDeepMergesMapsAndReplacesEverythingElse(t *testing.T) {
	dst := Values{
		"image":     map[string]any{"repository": "example.com/api", "tag": "1.0"},
		"resources": map[string]any{"memory": "128M"},
		"tags":      []any{"a", "b"},
	}
	src := Values{
		"image": map[string]any{"tag": "2.0"},
		"tags":  []any{"c"},
		"extra": "value",
	}
	out := MergeValues(dst, src)

	img, ok := asValues(out["image"])
	if !ok || img["repository"] != "example.com/api" || img["tag"] != "2.0" {
		t.Fatalf("image should merge key by key, keeping repository and overriding tag: %#v", out["image"])
	}
	if res, ok := asValues(out["resources"]); !ok || res["memory"] != "128M" {
		t.Errorf("resources untouched by src should survive: %#v", out["resources"])
	}
	tags, ok := out["tags"].([]any)
	if !ok || len(tags) != 1 || tags[0] != "c" {
		t.Errorf("a slice must be replaced wholesale, not appended: %#v", out["tags"])
	}
	if out["extra"] != "value" {
		t.Errorf("a key only src has must still land: %#v", out)
	}
	// dst itself must not have been mutated.
	if dst["image"].(map[string]any)["tag"] != "1.0" {
		t.Error("MergeValues must not mutate its dst argument")
	}
}

func TestLoadValuesFilesMergesLaterFilesWinning(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.yaml")
	b := filepath.Join(dir, "b.yaml")
	os.WriteFile(a, []byte("image:\n  repository: example.com/api\n  tag: \"1.0\"\nzone: dmz\n"), 0o644)
	os.WriteFile(b, []byte("image:\n  tag: \"2.0\"\n"), 0o644)

	v, err := LoadValuesFiles(a, b)
	if err != nil {
		t.Fatal(err)
	}
	img, ok := asValues(v["image"])
	if !ok || img["repository"] != "example.com/api" || img["tag"] != "2.0" || v["zone"] != "dmz" {
		t.Fatalf("unexpected merge result: %#v", v)
	}
}

func TestLoadValuesFileMissingIsAnError(t *testing.T) {
	if _, err := LoadValuesFile(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("a missing values file should be an error, not silently empty")
	}
}

func TestRenderTemplateSubstitutesAndDefaults(t *testing.T) {
	values := Values{"image": map[string]any{"tag": "1.2.3"}}
	out, err := renderTemplate("t", []byte("tag: {{ .Values.image.tag }}\nlevel: {{ default \"info\" .Values.logLevel }}\n"), values)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); got != "tag: 1.2.3\nlevel: info\n" {
		t.Fatalf("got %q", got)
	}
}

func TestRenderTemplateRequiredCatchesAMissingValueAsAFunctionArgument(t *testing.T) {
	// A chained lookup that is only printed (never passed to a function)
	// silently becomes the literal "<no value>" - that is a real gap
	// `required` closes: as a function argument, the same missing chain
	// resolves to nil, so `required` can catch it before it reaches a document.
	_, err := renderTemplate("t", []byte(`{{ required "tag is required" .Values.image.tag }}`), Values{})
	if err == nil || !strings.Contains(err.Error(), "tag is required") {
		t.Fatalf("want a required error, got %v", err)
	}

	out, err := renderTemplate("t", []byte(`{{ required "tag is required" .Values.image.tag }}`), Values{"image": map[string]any{"tag": "2.0"}})
	if err != nil || string(out) != "2.0" {
		t.Fatalf("a present value should pass through: out=%q err=%v", out, err)
	}
}

func TestRenderTemplateBadSyntaxNamesTheFile(t *testing.T) {
	_, err := renderTemplate("myfile.yaml", []byte("{{ .Values.nope"), nil)
	if err == nil || !strings.Contains(err.Error(), "myfile.yaml") {
		t.Fatalf("want an error naming myfile.yaml, got %v", err)
	}
}

// TestTemplateIsDeclaredByFileNameNotContents is the rule everything else
// rests on: a *.tpl renders, a .yaml does not - however either one looks
// inside. "{{" in a plain document is just text, so a comment mentioning the
// syntax, or a value that genuinely contains braces, can never opt a file
// into templating by accident.
func TestTemplateIsDeclaredByFileNameNotContents(t *testing.T) {
	files := map[string]string{
		"templated.yaml.tpl": `
apiVersion: v1
kind: Pod
metadata: {name: web}
spec:
  containers:
    - name: web
      image: "{{ .Values.image.repository }}:{{ .Values.image.tag }}"
`,
		"plain.yaml": `
# This comment mentions {{ .Values }} and {{ if }} and that is fine.
apiVersion: v1
kind: Pod
metadata: {name: literal}
spec:
  containers:
    - name: literal
      image: "example.com/literal:{{ not a template }}"
`,
		"host.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: vm-1}
spec: {applications: [web, literal]}
`,
	}
	ix := loadIndex(t, files)
	if len(ix.Templates()) != 1 || len(ix.Pods) != 1 {
		t.Fatalf("want 1 template and 1 plain pod after load, got %d and %d", len(ix.Templates()), len(ix.Pods))
	}

	values := Values{"image": map[string]any{"repository": "example.com/web", "tag": "2.0"}}
	desired, err := ix.Resolve(context.Background(), ResolveOptions{Host: "vm-1", Values: values})
	if err != nil {
		t.Fatal(err)
	}
	if app, _ := desired.App("web"); app.Image != "example.com/web:2.0" {
		t.Fatalf("template was not rendered: %+v", app)
	}
	if app, _ := desired.App("literal"); app.Image != "example.com/literal:{{ not a template }}" {
		t.Fatalf("a plain .yaml must never be rendered; got %+v", app)
	}
}

// TestEnvironmentGroupAndHostValuesMergeInOverridePrecedence checks that a
// Host's own values:, a Group's, an Environment's, and the agent.yaml
// fallback (ResolveOptions.Values) merge with exactly the precedence
// overrides already use: environment < group < host, host winning.
func TestEnvironmentGroupAndHostValuesMergeInOverridePrecedence(t *testing.T) {
	files := map[string]string{
		"app.yaml.tpl": `
apiVersion: v1
kind: Pod
metadata: {name: web}
spec:
  containers:
    - name: web
      image: '{{ .Values.tag }}'
      env:
        - name: LOG
          value: '{{ .Values.log }}'
`,
		"env.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Environment
metadata: {name: prod}
spec: {applications: [web], values: [values/env.yaml]}
`,
		"group.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Group
metadata: {name: web}
spec: {values: [values/group.yaml]}
`,
		"host.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: vm-1}
spec: {environment: prod, groups: [web], values: [values/host.yaml]}
`,
		"values/env.yaml":   "tag: env-tag\n",
		"values/group.yaml": "tag: group-tag\n",
		"values/host.yaml":  "tag: host-tag\n",
	}
	ix := loadIndex(t, files)
	baseline := Values{"tag": "baseline-tag", "log": "baseline-log"}
	desired, err := ix.Resolve(context.Background(), ResolveOptions{Host: "vm-1", Values: baseline})
	if err != nil {
		t.Fatal(err)
	}
	app, ok := desired.App("web")
	if !ok || app.Image != "host-tag" {
		t.Fatalf("the host's own values should win: %+v", app)
	}
	if app.Env["LOG"] != "baseline-log" {
		t.Fatalf("a key no layer touches should still come from the agent.yaml fallback: %+v", app.Env)
	}
}

// TestTwoHostsSharingAnEnvironmentGetTheirOwnGroupValues mirrors the real
// multi-env shape: one Environment two hosts share, two Groups that each
// supply a different value for the same key.
func TestTwoHostsSharingAnEnvironmentGetTheirOwnGroupValues(t *testing.T) {
	files := map[string]string{
		"app.yaml.tpl": `
apiVersion: v1
kind: Pod
metadata: {name: web}
spec:
  containers:
    - name: web
      image: '{{ .Values.tag }}'
`,
		"env.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Environment
metadata: {name: prod}
spec: {applications: [web]}
`,
		"groups.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Group
metadata: {name: dmz}
spec: {values: [values/dmz.yaml]}
---
apiVersion: gitops.podcd.io/v1
kind: Group
metadata: {name: iso}
spec: {values: [values/iso.yaml]}
`,
		"hosts.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: dmz-host}
spec: {environment: prod, groups: [dmz]}
---
apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: iso-host}
spec: {environment: prod, groups: [iso]}
`,
		"values/dmz.yaml": "tag: dmz-tag\n",
		"values/iso.yaml": "tag: iso-tag\n",
	}
	ix := loadIndex(t, files)

	dmz, err := ix.Resolve(context.Background(), ResolveOptions{Host: "dmz-host"})
	if err != nil {
		t.Fatal(err)
	}
	if app, _ := dmz.App("web"); app.Image != "dmz-tag" {
		t.Fatalf("dmz-host should see dmz's value: %+v", app)
	}

	iso, err := ix.Resolve(context.Background(), ResolveOptions{Host: "iso-host"})
	if err != nil {
		t.Fatal(err)
	}
	if app, _ := iso.App("web"); app.Image != "iso-tag" {
		t.Fatalf("iso-host should see iso's value, not dmz's leaking across hosts: %+v", app)
	}
}

// TestTemplateMistakesSurfaceAtResolve: a template is not decoded until it
// is rendered, so a structural mistake in one (an unknown field, here) is
// reported when a host resolves, with the template named - not lost.
func TestTemplateMistakesSurfaceAtResolve(t *testing.T) {
	files := map[string]string{
		"app.yaml.tpl": `
apiVersion: v1
kind: Pod
metadata: {name: web}
spec:
  containers:
    - name: web
      image: '{{ .Values.tag }}'
      imagee: typo
`,
		"host.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: vm-1}
spec: {applications: [web]}
`,
	}
	ix := loadIndex(t, files) // loading must succeed: nothing is decided about a template yet
	_, err := ix.Resolve(context.Background(), ResolveOptions{Host: "vm-1", Values: Values{"tag": "x"}})
	if err == nil || !strings.Contains(err.Error(), "imagee") || !strings.Contains(err.Error(), "app.yaml.tpl") {
		t.Fatalf("want an error naming the unknown field and the template, got %v", err)
	}
}

// Each host sees its own branch.
func TestBlockConditionalsWork(t *testing.T) {
	files := map[string]string{
		"app.yaml.tpl": `
apiVersion: v1
kind: Pod
metadata:
  name: web
spec:
  containers:
    - name: web
      image: nginx
      env:
        - name: BASE
          value: always
      {{- if .Values.extra }}
        - name: EXTRA
          value: '{{ .Values.extra }}'
      {{- end }}
`,
		"hosts.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: with-extra}
spec: {applications: [web]}
---
apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: without-extra}
spec: {applications: [web]}
`,
	}
	ix := loadIndex(t, files)

	with, err := ix.Resolve(context.Background(), ResolveOptions{Host: "with-extra", Values: Values{"extra": "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if app, _ := with.App("web"); app.Env["EXTRA"] != "hi" {
		t.Fatalf("the true branch should set EXTRA: %+v", app.Env)
	}

	without, err := ix.Resolve(context.Background(), ResolveOptions{Host: "without-extra"})
	if err != nil {
		t.Fatal(err)
	}
	if app, _ := without.App("web"); app.Env["EXTRA"] != "" {
		t.Fatalf("the false branch should omit EXTRA entirely, not set it empty: %+v", app.Env)
	}
}

// TestTemplatedNameWorks: because a template's identity comes from what it renders to.
func TestTemplatedNameWorks(t *testing.T) {
	files := map[string]string{
		"app.yaml.tpl": `
apiVersion: v1
kind: Pod
metadata:
  name: '{{ .Values.name }}'
spec:
  containers:
    - name: web
      image: nginx
`,
		"host.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: vm-1}
spec: {applications: [from-values]}
`,
	}
	ix := loadIndex(t, files)
	desired, err := ix.Resolve(context.Background(), ResolveOptions{Host: "vm-1", Values: Values{"name": "from-values"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := desired.App("from-values"); !ok {
		t.Fatalf("the rendered name should be what the host finds: %v", desired.Names())
	}
}

// TestPodBlockConditionalWorks is TestBlockConditionalsWork for kind: Pod: a
// conditional around a whole container.
func TestPodBlockConditionalWorks(t *testing.T) {
	files := map[string]string{
		"pod.yaml.tpl": `
apiVersion: v1
kind: Pod
metadata:
  name: web
spec:
  containers:
    - name: app
      image: nginx
{{- if .Values.withSidecar }}
    - name: sidecar
      image: envoy
{{- end }}
`,
		"hosts.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: with-sidecar}
spec: {applications: [web]}
---
apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: without-sidecar}
spec: {applications: [web]}
`,
	}
	ix := loadIndex(t, files)

	with, err := ix.Resolve(context.Background(), ResolveOptions{Host: "with-sidecar", Values: Values{"withSidecar": true}})
	if err != nil {
		t.Fatal(err)
	}
	if app, _ := with.App("web"); len(app.Images) != 2 {
		t.Fatalf("want 2 containers, got %v", app.Images)
	}

	without, err := ix.Resolve(context.Background(), ResolveOptions{Host: "without-sidecar"})
	if err != nil {
		t.Fatal(err)
	}
	if app, _ := without.App("web"); len(app.Images) != 1 {
		t.Fatalf("want 1 container, got %v", app.Images)
	}
}

// TestConfigMapAndSecretTemplates: the two remaining deployable kinds are
// templates like any other. A Pod may refer to a ConfigMap or a Secret whose
// own body is templated, block conditionals included, rendered with the same
// per-host values as the Pod itself.
func TestConfigMapAndSecretTemplates(t *testing.T) {
	files := map[string]string{
		"pod.yaml": `
apiVersion: v1
kind: Pod
metadata:
  name: web
spec:
  containers:
    - name: app
      image: nginx
      envFrom:
        - configMapRef: {name: web-config}
      env:
        - name: CA_CERT
          valueFrom:
            secretKeyRef: {name: web-secret, key: ca.crt}
`,
		"configmap.yaml.tpl": `
apiVersion: v1
kind: ConfigMap
metadata:
  name: web-config
data:
  LOG_LEVEL: '{{ default "info" .Values.logLevel }}'
{{- if .Values.extra }}
  EXTRA: '{{ .Values.extra }}'
{{- end }}
`,
		"secret.yaml.tpl": `
apiVersion: v1
kind: Secret
metadata:
  name: web-secret
stringData:
  ca.crt: '{{ .Values.caCert }}'
`,
		"host.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: vm-1}
spec: {applications: [web]}
`,
	}
	ix := loadIndex(t, files)

	desired, err := ix.Resolve(context.Background(), ResolveOptions{Host: "vm-1", Values: Values{
		"logLevel": "debug", "extra": "hi", "caCert": "PEM-CHAIN",
	}})
	if err != nil {
		t.Fatal(err)
	}
	app, ok := desired.App("web")
	if !ok {
		t.Fatal("web not resolved")
	}
	manifest := string(app.Manifest)
	if !strings.Contains(manifest, "LOG_LEVEL: debug") || !strings.Contains(manifest, "EXTRA: hi") {
		t.Fatalf("templated + conditional ConfigMap data missing from manifest:\n%s", manifest)
	}
	if !strings.Contains(manifest, "PEM-CHAIN") {
		t.Fatalf("templated Secret value missing from manifest:\n%s", manifest)
	}
}

// TestTemplateMayNotRenderAHostGroupOrEnvironment: those decide which values
// a host gets, so they cannot themselves come out of a template.
func TestTemplateMayNotRenderAHostGroupOrEnvironment(t *testing.T) {
	files := map[string]string{
		"group.yaml.tpl": `
apiVersion: gitops.podcd.io/v1
kind: Group
metadata: {name: web}
spec: {applications: [nginx]}
`,
		"host.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: vm-1}
spec: {applications: []}
`,
	}
	ix := loadIndex(t, files)
	_, err := ix.Resolve(context.Background(), ResolveOptions{Host: "vm-1"})
	if err == nil || !strings.Contains(err.Error(), "Group") || !strings.Contains(err.Error(), "template") {
		t.Fatalf("want a clear error about templating a Group, got %v", err)
	}
}

// TestTemplateMayNotShadowAPlainDocument: the "defined twice" rule holds
// across the two sets, so a template cannot quietly replace a written file.
func TestTemplateMayNotShadowAPlainDocument(t *testing.T) {
	files := map[string]string{
		"app.yaml": `
apiVersion: v1
kind: Pod
metadata: {name: web}
spec:
  containers:
    - name: web
      image: nginx
`,
		"app.yaml.tpl": `
apiVersion: v1
kind: Pod
metadata: {name: web}
spec:
  containers:
    - name: web
      image: '{{ .Values.image }}'
`,
		"host.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: vm-1}
spec: {applications: [web]}
`,
	}
	ix := loadIndex(t, files)
	_, err := ix.Resolve(context.Background(), ResolveOptions{Host: "vm-1", Values: Values{"image": "x"}})
	if err == nil || !strings.Contains(err.Error(), "defined twice") {
		t.Fatalf("want a defined-twice error, got %v", err)
	}
}

// TestMissingValuesFileIsAClearResolveError checks that a Host naming a
// values file nobody loaded fails with a message pointing at the file and
// the layer that named it, not a generic "not found".
func TestMissingValuesFileIsAClearResolveError(t *testing.T) {
	files := map[string]string{
		"app.yaml": `
apiVersion: v1
kind: Pod
metadata: {name: web}
spec:
  containers:
    - name: web
      image: nginx
`,
		"host.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: vm-1}
spec: {applications: [web], values: [values/missing.yaml]}
`,
	}
	ix := loadIndex(t, files)
	_, err := ix.Resolve(context.Background(), ResolveOptions{Host: "vm-1"})
	if err == nil || !strings.Contains(err.Error(), "values/missing.yaml") || !strings.Contains(err.Error(), "host/vm-1") {
		t.Fatalf("want an error naming the file and the layer, got %v", err)
	}
}
