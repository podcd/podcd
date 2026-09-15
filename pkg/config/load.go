package config

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	sigyaml "sigs.k8s.io/yaml"

	"github.com/podcd/podcd/pkg/secrets"
)

// Index is every document the agent loaded, addressed by name and kind.
//
// Names are global across repositories on purpose.
// Two repositories defining the same Application is an ambiguity, and ambiguity is an error here, not a coin flip.
// Applications and Pods share one namespace too, because a host lists both under `applications:`.
type Index struct {
	Applications map[string]Doc[AppSpec]
	Groups       map[string]Doc[SelectionSpec]
	Environments map[string]Doc[SelectionSpec]
	Hosts        map[string]Doc[HostSpec]

	// A Secret's values must be references (env:NAME, file:path), never
	// plaintext; the loader enforces that.
	Pods       map[string]Doc[corev1.Pod]
	ConfigMaps map[string]Doc[corev1.ConfigMap]
	Secrets    map[string]Doc[corev1.Secret]

	// Values is available to every document as `.Values` when the document
	// is templated (see renderTemplate). Set it before loading any tree or
	// bytes; it applies to everything loaded afterwards.
	Values Values
}

// NewIndex returns an empty index.
func NewIndex() *Index {
	return &Index{
		Applications: map[string]Doc[AppSpec]{},
		Groups:       map[string]Doc[SelectionSpec]{},
		Environments: map[string]Doc[SelectionSpec]{},
		Hosts:        map[string]Doc[HostSpec]{},
		Pods:         map[string]Doc[corev1.Pod]{},
		ConfigMaps:   map[string]Doc[corev1.ConfigMap]{},
		Secrets:      map[string]Doc[corev1.Secret]{},
	}
}

// HostNames returns the known host names, sorted.
func (ix *Index) HostNames() []string { return slices.Sorted(maps.Keys(ix.Hosts)) }

// LoadTree walks one repository checkout and adds every document it finds.
//
// Files are visited in sorted path order, so load order never depends on the filesystem.
// Only .yaml and .yml are read, everything else (READMEs, scripts, Containerfiles) is ignored.
// Dot-directories are skipped.
func (ix *Index) LoadTree(repo, root string) error {
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(name, ".") {
			return nil
		}
		if ext := strings.ToLower(filepath.Ext(name)); ext == ".yaml" || ext == ".yml" {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("walking %s: %w", root, err)
	}
	slices.Sort(files)

	for _, path := range files {
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("reading %s: %w", path, readErr)
		}
		if err := ix.loadFile(repo, rel, data); err != nil {
			return err
		}
	}
	return nil
}

func (ix *Index) LoadBytes(repo, path string, data []byte) error {
	return ix.loadFile(repo, path, data)
}

func SplitDocuments(repo, path string, data []byte) ([]Source, error) {
	// The reader hands back lines re-terminated with "\n", so normalise the
	// input the same way and every chunk is a verbatim slice of it.
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	if len(data) > 0 && !bytes.HasSuffix(data, []byte("\n")) {
		data = append(data, '\n')
	}
	r := k8syaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	var docs []Source
	offset := 0
	for {
		raw, err := r.Read()
		if errors.Is(err, io.EOF) {
			return docs, nil
		}
		if err != nil {
			return nil, err
		}
		// The chunk is a verbatim slice of the input, so its position is where it next occurs after the previous one;
		// reported at its first content line rather than at a separator, blank line or comment.
		at := offset + bytes.Index(data[offset:], raw)
		offset = at + len(raw)
		if skip := leadingBlank(raw); skip >= 0 {
			docs = append(docs, Source{Repo: repo, Path: path, Line: bytes.Count(data[:at], []byte("\n")) + 1 + skip, Raw: raw})
		}
	}
}

// leadingBlank counts the blank and comment lines before a document's first content line, or returns -1 when there is no content at all.
func leadingBlank(raw []byte) int {
	for i, l := range bytes.Split(raw, []byte("\n")) {
		if t := bytes.TrimSpace(l); len(t) > 0 && t[0] != '#' {
			return i
		}
	}
	return -1
}

// loadFile decodes one possibly multi-document YAML file into the index.
// Each document is decoded strictly against its real Go type.
// An unknown field is an error whichever family the document belongs to.
func (ix *Index) loadFile(repo, path string, data []byte) error {
	// A document opts into templating by using "{{" at all, so a file that
	// never does costs nothing and cannot be broken by a template error.
	if bytes.Contains(data, []byte("{{")) {
		rendered, err := renderTemplate(Source{Repo: repo, Path: path}.String(), data, ix.Values)
		if err != nil {
			return err
		}
		data = rendered
	}
	docs, err := SplitDocuments(repo, path, data)
	if err != nil {
		return fmt.Errorf("%s/%s: %w", repo, path, err)
	}
	for _, src := range docs {
		var env document
		if err := sigyaml.Unmarshal(src.Raw, &env); err != nil {
			return fmt.Errorf("%s: %w", src, err)
		}
		if env.APIVersion == "" && env.Kind == "" {
			continue // not one of ours; ignore quietly
		}
		if env.Name == "" {
			return fmt.Errorf("%s: kind %s has no metadata.name", src, env.Kind)
		}
		if err := ix.add(env, src); err != nil {
			return err
		}
	}
	return nil
}

func (ix *Index) add(env document, src Source) error {
	name := env.Name
	switch env.APIVersion {
	case APIVersion:
		switch env.Kind {
		case KindApplication:
			if prev, ok := ix.Pods[name]; ok {
				return fmt.Errorf("%q is both an Application (%s) and a Pod (%s); a host lists both under applications, so the name must be unique", name, src, prev.Source)
			}
			return addSpec(ix.Applications, env.Kind, name, src)
		case KindGroup:
			return addSpec(ix.Groups, env.Kind, name, src)
		case KindEnvironment:
			return addSpec(ix.Environments, env.Kind, name, src)
		case KindHost:
			return addSpec(ix.Hosts, env.Kind, name, src)
		}
		return fmt.Errorf("%s: unknown kind %q in %s", src, env.Kind, APIVersion)
	case CoreAPIVersion:
		switch env.Kind {
		case KindPod:
			if prev, ok := ix.Applications[name]; ok {
				return fmt.Errorf("%q is both a Pod (%s) and an Application (%s); a host lists both under applications, so the name must be unique", name, src, prev.Source)
			}
			return addObject(ix.Pods, env.Kind, name, src, nil)
		case KindConfigMap:
			return addObject(ix.ConfigMaps, env.Kind, name, src, nil)
		case KindSecret:
			return addObject(ix.Secrets, env.Kind, name, src, checkSecretIsReferenceOnly)
		}
		return fmt.Errorf("%s: kind %q is not supported from apiVersion v1 (Pod, ConfigMap, Secret are)", src, env.Kind)
	default:
		return fmt.Errorf("%s: apiVersion %q is not supported (want %s or %s)", src, env.APIVersion, APIVersion, CoreAPIVersion)
	}
}

// addSpec decodes the spec of one of podcd's kinds and indexes it.
func addSpec[T any](into map[string]Doc[T], kind, name string, src Source) error {
	var doc struct {
		metav1.TypeMeta   `json:",inline"`
		metav1.ObjectMeta `json:"metadata"`
		Spec              T `json:"spec"`
	}
	if err := strictDecode(&doc, src, kind); err != nil {
		return err
	}
	return put(into, kind, name, src, doc.Spec)
}

// addObject decodes a whole core/v1 object and indexes it, after an optional
// extra check.
func addObject[T any](into map[string]Doc[T], kind, name string, src Source, check func(T, Source) error) error {
	var obj T
	if err := strictDecode(&obj, src, kind); err != nil {
		return err
	}
	if check != nil {
		if err := check(obj, src); err != nil {
			return err
		}
	}
	return put(into, kind, name, src, obj)
}

func put[T any](into map[string]Doc[T], kind, name string, src Source, spec T) error {
	if prev, ok := into[name]; ok {
		return fmt.Errorf("%s %q is defined twice: %s and %s (names must be unique across all repositories)",
			kind, name, prev.Source, src)
	}
	into[name] = Doc[T]{Name: name, Spec: spec, Source: src}
	return nil
}

// checkSecretIsReferenceOnly enforces the one rule that does not bend: a Secret in Git carries references to values, never the values.
//
// `data` is base64 of plaintext and is refused outright.
// `stringData` values must look like "scheme:locator", they are resolved on the host at reconcile time by the secrets provider.
func checkSecretIsReferenceOnly(sec corev1.Secret, src Source) error {
	if len(sec.Data) > 0 {
		return fmt.Errorf("%s: Secret %q has plaintext values in data (%s); use stringData with references such as env:NAME or file:path",
			src, sec.Name, strings.Join(slices.Sorted(maps.Keys(sec.Data)), ", "))
	}
	var literals []string
	for k, v := range sec.StringData {
		if !secrets.IsReference(v) {
			literals = append(literals, k)
		}
	}
	if len(literals) > 0 {
		slices.Sort(literals)
		return fmt.Errorf("%s: Secret %q: stringData values must be references like env:NAME or file:path, not literals (keys: %s)",
			src, sec.Name, strings.Join(literals, ", "))
	}
	return nil
}

// strictDecode decodes a YAML document against its real Go type and rejects unknown fields.
// A misspelled key is a mistake worth failing on: silently ignoring `imagee:` would leave a host running the wrong thing.
func strictDecode(out any, src Source, kind string) error {
	if err := sigyaml.UnmarshalStrict(src.Raw, out); err != nil {
		return fmt.Errorf("%s: kind %s: %w", src, kind, err)
	}
	return nil
}
