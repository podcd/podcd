// Values templating lets one set of Application/Pod documents in Git serve
// several hosts, by filling in the parts that differ (an image tag, a
// resource limit, a domain) from values chosen by a Host's own Environment,
// Groups and Host document - the same precedence overrides already use -
// with an agent.yaml `repositories[].values` list as a last-resort, host-local
// fallback beneath all of that.
//
// A template is a file named *.tpl (edge-api.yaml.tpl, say). Nothing about
// its contents makes it one, and nothing about a plain .yaml's contents makes
// it a template - "{{" there is just text. A template is rendered in Resolve,
// once a host and that host's values are known, and only then decoded, so it
// may use the whole of text/template: conditionals around entire keys,
// ranges, a name computed from a value.
// Index.renderTemplates in resolve.go.
package config

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"strconv"
	"strings"
	"text/template"

	sigyaml "sigs.k8s.io/yaml"
)

// Values is the nested data a document's template sees as `.Values`.
// It is decoded from YAML, so maps are keyed by string and any scalar,
// sequence or nested map JSON/YAML can express is valid.
type Values map[string]any

// LoadValuesFile reads one YAML values file from disk.
func LoadValuesFile(path string) (Values, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading values %s: %w", path, err)
	}
	return parseValues(path, data)
}

// parseValues decodes YAML bytes already in hand - shared by LoadValuesFile
// (reads from disk) and Index.readValuesFile (reads from the loader's own
// cache of every file it saw, so a Host/Group/Environment's own `values:`
// list never touches disk again after the initial load).
func parseValues(name string, data []byte) (Values, error) {
	var v Values
	if err := sigyaml.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("values %s: %w", name, err)
	}
	return v, nil
}

// LoadValuesFiles reads each file in order and merges them, later files overriding earlier ones
func LoadValuesFiles(paths ...string) (Values, error) {
	merged := Values{}
	for _, p := range paths {
		v, err := LoadValuesFile(p)
		if err != nil {
			return nil, err
		}
		merged = MergeValues(merged, v)
	}
	return merged, nil
}

// MergeValues layers src over dst: maps are merged key by key, recursively;
// anything else (including a slice) is replaced wholesale.
func MergeValues(dst, src Values) Values {
	if dst == nil {
		dst = Values{}
	}
	out := maps.Clone(dst)
	for k, sv := range src {
		if dv, ok := out[k]; ok {
			if dm, ok := asValues(dv); ok {
				if sm, ok := asValues(sv); ok {
					out[k] = MergeValues(dm, sm)
					continue
				}
			}
		}
		out[k] = sv
	}
	return out
}

// asValues reports whether v decoded as a nested mapping, whichever of the
// two shapes sigs.k8s.io/yaml produces for it.
func asValues(v any) (Values, bool) {
	switch m := v.(type) {
	case Values:
		return m, true
	case map[string]any:
		return m, true
	default:
		return nil, false
	}
}

// templateFuncs are the handful of helpers a values template actually needs.
// This deliberately is not sprig: podcd stays a single small dependency-light
// binary, and a missing value is meant to be visible, not silently smoothed
// over by a large function library.
var templateFuncs = template.FuncMap{
	"default": func(def, val any) any {
		if val == nil {
			return def
		}
		if s, ok := val.(string); ok && s == "" {
			return def
		}
		return val
	},
	// required stops the render with msg when val is missing, rather than
	// silently writing the literal "<no value>" into the document - a
	// missing map key is not an error by itself, so a template that truly
	// needs a value must say so.
	"required": func(msg string, val any) (any, error) {
		if val == nil {
			return nil, fmt.Errorf("%s", msg)
		}
		if s, ok := val.(string); ok && s == "" {
			return nil, fmt.Errorf("%s", msg)
		}
		return val, nil
	},
	"upper":      strings.ToUpper,
	"lower":      strings.ToLower,
	"trim":       strings.TrimSpace,
	"trimPrefix": func(prefix, s string) string { return strings.TrimPrefix(s, prefix) },
	"trimSuffix": func(suffix, s string) string { return strings.TrimSuffix(s, suffix) },
	"replace":    func(old, new, s string) string { return strings.ReplaceAll(s, old, new) },
	"quote":      func(v any) string { return strconv.Quote(fmt.Sprint(v)) },
}

// renderTemplate expands {{ .Values... }} in a document against values.
// name is used only for error messages (repo/path), so a broken template
// points a human at the file that needs fixing.
func renderTemplate(name string, data []byte, values Values) ([]byte, error) {
	tmpl, err := template.New(name).Funcs(templateFuncs).Parse(string(data))
	if err != nil {
		return nil, fmt.Errorf("%s: template: %w", name, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]any{"Values": values}); err != nil {
		return nil, fmt.Errorf("%s: template: %w", name, err)
	}
	return buf.Bytes(), nil
}
