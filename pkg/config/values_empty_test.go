package config

import (
	"context"
	"strings"
	"testing"
)

// A template whose output is empty - the Helm idiom of wrapping a whole
// resource in {{ if .Values.enabled }} - is not an error and defines
// nothing. Whitespace, comments, a lone "---" and a YAML null are all
// "nothing" too. What decides whether the host still needs the document is
// the Host/Group/Environment list, which is plain YAML and cannot be
// templated away, so a host that lists a switched-off application is told.
func TestTemplateRenderingToNothingDefinesNothing(t *testing.T) {
	for name, tpl := range map[string]string{
		"empty":         ``,
		"whitespace":    "\n  \n\t\n",
		"comment":       "# nothing here\n",
		"separator":     "---\n",
		"separators":    "---\n---\n",
		"null":          "null\n",
		"if-false":      "{{ if .Values.enabled }}\napiVersion: v1\nkind: Pod\nmetadata: {name: web}\nspec: {containers: [{name: w, image: x}]}\n{{ end }}\n",
		"if-false-dash": "{{- if .Values.enabled }}\n---\napiVersion: v1\nkind: Pod\nmetadata: {name: web}\nspec: {containers: [{name: w, image: x}]}\n{{- end }}\n",
	} {
		t.Run(name, func(t *testing.T) {
			ix := loadIndex(t, map[string]string{
				"web.yaml.tpl": tpl,
				"host.yaml":    "apiVersion: gitops.podcd.io/v1\nkind: Host\nmetadata: {name: vm-1}\nspec: {applications: []}\n",
			})
			desired, err := ix.Resolve(context.Background(), ResolveOptions{Host: "vm-1"})
			if err != nil {
				t.Fatalf("a template that renders to nothing must not fail the host: %v", err)
			}
			if len(desired.Applications) != 0 {
				t.Fatalf("nothing should be defined, got %+v", desired.Applications)
			}
		})
	}
}

func TestTemplateSwitchedOffButStillListedIsAnError(t *testing.T) {
	ix := loadIndex(t, map[string]string{
		"web.yaml.tpl": "{{ if .Values.enabled }}\napiVersion: v1\nkind: Pod\nmetadata: {name: web}\nspec: {containers: [{name: w, image: x}]}\n{{ end }}\n",
		"host.yaml":    "apiVersion: gitops.podcd.io/v1\nkind: Host\nmetadata: {name: vm-1}\nspec: {applications: [web]}\n",
	})
	_, err := ix.Resolve(context.Background(), ResolveOptions{Host: "vm-1"})
	if err == nil || !strings.Contains(err.Error(), "web") {
		t.Fatalf("the host lists web but nothing rendered it; that must be said: %v", err)
	}
	// Switch it on and the same host resolves.
	desired, err := ix.Resolve(context.Background(), ResolveOptions{Host: "vm-1", Values: Values{"enabled": true}})
	if err != nil || len(desired.Applications) != 1 {
		t.Fatalf("enabled: %v %+v", err, desired.Applications)
	}
}

// Rendering to nothing is a per-host outcome: the same template can define
// the Pod for one host and nothing for another.
func TestTemplateCanRenderToNothingForOneHostOnly(t *testing.T) {
	ix := loadIndex(t, map[string]string{
		"web.yaml.tpl": "{{ if .Values.enabled }}\napiVersion: v1\nkind: Pod\nmetadata: {name: web}\nspec: {containers: [{name: w, image: x}]}\n{{ end }}\n",
		"hosts.yaml": `apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: vm-on}
spec:
  applications: [web]
  values: [values/on.yaml]
---
apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: vm-off}
spec: {applications: []}
`,
		"values/on.yaml": "enabled: true\n",
	})
	for host, want := range map[string]int{"vm-on": 1, "vm-off": 0} {
		desired, err := ix.Resolve(context.Background(), ResolveOptions{Host: host})
		if err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		if len(desired.Applications) != want {
			t.Fatalf("%s: want %d applications, got %+v", host, want, desired.Applications)
		}
	}
}
