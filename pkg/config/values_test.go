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

// TestLoaderTemplatesOnlyFilesThatOptIn is an end-to-end check: a repository
// with a plain file and a templated one, loaded and resolved for a host,
// produces the value that came from the values file - and a file that never
// writes "{{" is never even parsed as a template.
func TestLoaderTemplatesOnlyFilesThatOptIn(t *testing.T) {
	files := map[string]string{
		"app.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Application
metadata: {name: web}
spec:
  image: "{{ .Values.image.repository }}:{{ .Values.image.tag }}"
`,
		"host.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: vm-1}
spec: {applications: [web]}
`,
	}
	dir := writeTree(t, files)
	ix := NewIndex()
	if err := ix.LoadTree("test", dir); err != nil {
		t.Fatal(err)
	}
	values := Values{"image": map[string]any{"repository": "example.com/web", "tag": "2.0"}}
	desired, err := ix.Resolve(context.Background(), ResolveOptions{Host: "vm-1", Values: values})
	if err != nil {
		t.Fatal(err)
	}
	app, ok := desired.App("web")
	if !ok || app.Image != "example.com/web:2.0" {
		t.Fatalf("template was not applied: %+v", app)
	}
}

// TestEnvironmentGroupAndHostValuesMergeInOverridePrecedence checks that a
// Host's own values:, a Group's, an Environment's, and the agent.yaml
// fallback (ResolveOptions.Values) merge with exactly the precedence
// overrides already use: environment < group < host, host winning.
func TestEnvironmentGroupAndHostValuesMergeInOverridePrecedence(t *testing.T) {
	files := map[string]string{
		"app.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Application
metadata: {name: web}
spec:
  image: '{{ .Values.tag }}'
  env:
    LOG: '{{ .Values.log }}'
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
		"app.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Application
metadata: {name: web}
spec: {image: '{{ .Values.tag }}'}
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

// TestTemplatedApplicationSkipsEagerDecodeButStillValidatesAtResolve checks
// that a templated Application's structural mistakes (here, an unknown
// field) are not silently lost by deferring its decode to Resolve - they
// still surface, just when a host actually selects it, not at load time.
func TestTemplatedApplicationSkipsEagerDecodeButStillValidatesAtResolve(t *testing.T) {
	files := map[string]string{
		"app.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Application
metadata: {name: web}
spec:
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
	// Loading must not fail eagerly - the document contains "{{", so its
	// spec decode is deferred - but Resolve must still catch the typo.
	ix := loadIndex(t, files)
	if _, err := ix.Resolve(context.Background(), ResolveOptions{Host: "vm-1", Values: Values{"tag": "x"}}); err == nil || !strings.Contains(err.Error(), "imagee") {
		t.Fatalf("want an error naming the unknown field, got %v", err)
	}
}

// TestBlockConditionalsAreNotValidTemplatingBreakOtherHosts documents a real
// limitation: a template may only fill in a value inside an already-quoted
// string. A block conditional that adds or removes a whole line makes the
// raw, unrendered document invalid YAML, and every document is decoded once
// - structurally, before any host's values are known - so it breaks loading
// for every host, not just the one that would have taken the "if" branch.
func TestBlockConditionalsBreakLoadingForEveryHost(t *testing.T) {
	files := map[string]string{
		"app.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Application
metadata: {name: web}
spec:
  image: nginx
  env:
{{- if .Values.extra }}
    EXTRA: '{{ .Values.extra }}'
{{- end }}
`,
		"host.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: vm-1}
spec: {applications: [web]}
`,
	}
	dir := writeTree(t, files)
	ix := NewIndex()
	if err := ix.LoadTree("test", dir); err == nil {
		t.Fatal("a block conditional makes the raw document invalid YAML; loading must fail, not silently misparse")
	}
}

// TestMissingValuesFileIsAClearResolveError checks that a Host naming a
// values file nobody loaded fails with a message pointing at the file and
// the layer that named it, not a generic "not found".
func TestMissingValuesFileIsAClearResolveError(t *testing.T) {
	files := map[string]string{
		"app.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Application
metadata: {name: web}
spec: {image: nginx}
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
