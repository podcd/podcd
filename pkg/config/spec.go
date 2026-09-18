package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

func WriteAgentConfig(path string, cfg AgentConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	return os.WriteFile(path, []byte(RenderAgentConfig(cfg)), 0o644)
}

// RenderAgentConfig returns the annotated YAML for cfg.
func RenderAgentConfig(cfg AgentConfig) string {
	var b strings.Builder
	b.WriteString("# podcd agent configuration.\n")
	b.WriteString("# Lines starting with # show the default; uncomment one to change it.\n")
	renderStruct(&b, reflect.ValueOf(cfg), reflect.ValueOf(DefaultAgentConfig()), "", true)
	return b.String()
}

// renderStruct writes the fields of v, using def for "is this the default?"
// and for the commented value. top-level fields get a blank line before them.
func renderStruct(b *strings.Builder, v, def reflect.Value, indent string, top bool) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		key := yamlKey(f)
		if key == "" {
			continue
		}
		if top {
			b.WriteString("\n")
		}
		writeComment(b, indent, f.Tag.Get("doc"))
		fv, dv := v.Field(i), def.Field(i)

		switch {
		case f.Type.Kind() == reflect.Struct && f.Type != reflect.TypeOf(time.Duration(0)):
			fmt.Fprintf(b, "%s%s:\n", indent, key)
			renderStruct(b, fv, reflect.Zero(f.Type), indent+"  ", false)
		case f.Type.Kind() == reflect.Slice:
			renderScalarSlice(b, key, fv, indent)
		case f.Type.Kind() == reflect.Pointer && f.Type.Elem().Kind() == reflect.Struct:
			if fv.IsNil() {
				renderExample(b, key, f.Type.Elem(), indent)
				continue
			}
			fmt.Fprintf(b, "%s%s:\n", indent, key)
			renderStruct(b, fv.Elem(), reflect.Zero(f.Type.Elem()), indent+"  ", false)
		default:
			val, dflt := scalar(fv), scalar(dv)
			if d := f.Tag.Get("default"); d != "" {
				dflt = d
			}
			if !fv.IsZero() && val != dflt {
				fmt.Fprintf(b, "%s%s: %s\n", indent, key, yamlScalar(val))
			} else {
				fmt.Fprintf(b, "%s# %s: %s\n", indent, key, yamlScalar(dflt))
			}
		}
	}
}

// renderScalarSlice writes a list of plain values (strings, numbers): active
// with its items when set, a single commented placeholder when empty, the
// same "shown either way" rule scalar fields follow.
func renderScalarSlice(b *strings.Builder, key string, v reflect.Value, indent string) {
	if v.Len() == 0 {
		fmt.Fprintf(b, "%s# %s: []\n", indent, key)
		return
	}
	fmt.Fprintf(b, "%s%s:\n", indent, key)
	for i := 0; i < v.Len(); i++ {
		fmt.Fprintf(b, "%s  - %s\n", indent, yamlScalar(scalar(v.Index(i))))
	}
}

// renderExample writes an optional section that is not set, as a commented
// block of the fields that have an example tag.
func renderExample(b *strings.Builder, key string, t reflect.Type, indent string) {
	fmt.Fprintf(b, "%s# %s:\n", indent, key)
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		k, ex := yamlKey(f), f.Tag.Get("example")
		if k == "" || ex == "" {
			continue
		}
		fmt.Fprintf(b, "%s#   %s: %s\n", indent, k, ex)
	}
}

// writeComment word-wraps a doc string as "# " lines at the given indent.
func writeComment(b *strings.Builder, indent, doc string) {
	if doc == "" {
		return
	}
	const width = 76
	line := ""
	for _, word := range strings.Fields(doc) {
		if line != "" && len(indent)+2+len(line)+1+len(word) > width {
			fmt.Fprintf(b, "%s# %s\n", indent, line)
			line = ""
		}
		if line != "" {
			line += " "
		}
		line += word
	}
	fmt.Fprintf(b, "%s# %s\n", indent, line)
}

// Setting is one "path=value" change for EditAgentConfig. Paths are yaml
// paths: "host", "interval", "repository.url", "vault.address". A few
// short aliases exist for the repository's most-edited fields.
type Setting struct {
	Path  string
	Value string
}

var setAliases = map[string]string{
	"repo-url":  "repository.url",
	"repo-name": "repository.name",
	"repo-path": "repository.path",
	"revision":  "repository.revision",
}

func setByPath(v reflect.Value, path []string, value string) error {
	if len(path) == 0 {
		return setScalar(v, value)
	}
	head := path[0]
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		return setByPath(v.Elem(), path, value)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if yamlKey(v.Type().Field(i)) == head {
				return setByPath(v.Field(i), path[1:], value)
			}
		}
		return fmt.Errorf("no field %q (fields: %s)", head, strings.Join(fieldKeys(v.Type()), ", "))
	case reflect.Slice:
		idx, err := strconv.Atoi(head)
		if err != nil || idx < 0 {
			return fmt.Errorf("%q is not a list index", head)
		}
		for v.Len() <= idx {
			v.Set(reflect.Append(v, reflect.Zero(v.Type().Elem())))
		}
		return setByPath(v.Index(idx), path[1:], value)
	default:
		return fmt.Errorf("%q has no field %q", v.Type(), head)
	}
}

func setScalar(v reflect.Value, value string) error {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		return setScalar(v.Elem(), value)
	}
	switch v.Interface().(type) {
	case time.Duration:
		d, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("%q is not a duration (try 30s, 5m)", value)
		}
		v.SetInt(int64(d))
		return nil
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(value)
	case reflect.Bool:
		b, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("%q is not true or false", value)
		}
		v.SetBool(b)
	case reflect.Int, reflect.Int64:
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("%q is not a number", value)
		}
		v.SetInt(int64(n))
	default:
		return fmt.Errorf("cannot set a %s from the command line; edit the file", v.Kind())
	}
	return nil
}

// yamlKey is the key a field is written under, or "" if it is not written.
func yamlKey(f reflect.StructField) string {
	key, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
	if key == "-" {
		return ""
	}
	return key
}

func fieldKeys(t reflect.Type) []string {
	var keys []string
	for i := 0; i < t.NumField(); i++ {
		if k := yamlKey(t.Field(i)); k != "" {
			keys = append(keys, k)
		}
	}
	return keys
}

// scalar renders a field value for comparison and output: durations as
// "1m0s", pointers dereferenced, nil as "".
func scalar(v reflect.Value) string {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return ""
		}
		v = v.Elem()
	}
	if d, ok := v.Interface().(time.Duration); ok {
		return d.String()
	}
	return fmt.Sprint(v.Interface())
}

// yamlScalar renders a string the way yaml.v3 would inside a document, so a
// value that needs quoting gets it. Numbers, booleans and durations are
// passed as their text and come out bare.
func yamlScalar(s string) string {
	switch s {
	case "":
		return `""`
	case "true", "false":
		return s
	}
	if _, err := strconv.Atoi(s); err == nil {
		return s
	}
	if _, err := time.ParseDuration(s); err == nil {
		return s
	}
	out, err := yaml.Marshal(s)
	if err != nil {
		return s
	}
	return strings.TrimRight(string(out), "\n")
}

// cfgType is the AgentConfig type, for tests and tooling that enumerate fields.
func cfgType() reflect.Type { return reflect.TypeOf(AgentConfig{}) }
