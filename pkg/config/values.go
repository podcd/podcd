// Values templating: *.tpl files are rendered per host (Index.renderTemplates)
// with values from its Environment, Groups and Host (override precedence),
// above agent.yaml's repository.values, and only then decoded.
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

// Values is what a template sees as `.Values`.
type Values map[string]any

// LoadValuesFile reads one YAML values file from disk.
func LoadValuesFile(path string) (Values, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading values %s: %w", path, err)
	}
	return parseValues(path, data)
}

// parseValues decodes a values file's YAML.
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

// asValues reports whether v is a mapping, in either shape sigs.k8s.io/yaml produces.
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

// templateFuncs is a deliberately small helper set (not sprig).
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
	// required fails the render with msg instead of writing "<no value>".
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

// renderTemplate renders a template against values; name is for error messages.
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
