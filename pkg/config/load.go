package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	sigyaml "sigs.k8s.io/yaml"

	"gopkg.in/yaml.v3"
)

// Index is every document the agent loaded, addressed by name and kind.
//
// Names are global across repositories on purpose: two repositories defining
// the same Application is an ambiguity, and ambiguity is an error here, not a
// coin flip. Applications and Pods share one namespace too, because a host
// lists both under `applications:`.
type Index struct {
	Applications map[string]ApplicationDoc
	Groups       map[string]GroupDoc
	Environments map[string]EnvironmentDoc
	Hosts        map[string]HostDoc

	Pods       map[string]PodDoc
	ConfigMaps map[string]ConfigMapDoc
	Secrets    map[string]SecretDoc
}

// NewIndex returns an empty index.
func NewIndex() *Index {
	return &Index{
		Applications: map[string]ApplicationDoc{},
		Groups:       map[string]GroupDoc{},
		Environments: map[string]EnvironmentDoc{},
		Hosts:        map[string]HostDoc{},
		Pods:         map[string]PodDoc{},
		ConfigMaps:   map[string]ConfigMapDoc{},
		Secrets:      map[string]SecretDoc{},
	}
}

// HostNames returns the known host names, sorted.
func (ix *Index) HostNames() []string { return sortedKeys(ix.Hosts) }

// LoadTree walks one repository checkout and adds every document it finds.
//
// Files are visited in sorted path order so that load order never depends on
// the filesystem. Only .yaml and .yml are read; everything else (READMEs,
// scripts, Containerfiles) is ignored. Dot-directories are skipped.
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
	sort.Strings(files)

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

// loadFile decodes one possibly multi-document YAML file into the index.
//
// yaml.v3 splits the stream and keeps line numbers; each document is then
// re-encoded and decoded strictly against its real Go type, so an unknown
// field is an error whichever family the document belongs to.
func (ix *Index) loadFile(repo, path string, data []byte) error {
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	for i := 0; ; i++ {
		var node yaml.Node
		err := dec.Decode(&node)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s/%s: document %d: %w", repo, path, i, err)
		}
		if node.Kind == 0 || (node.Kind == yaml.DocumentNode && len(node.Content) == 0) {
			continue // empty document, e.g. a trailing ---
		}
		raw, err := yaml.Marshal(&node)
		if err != nil {
			return fmt.Errorf("%s/%s: document %d: %w", repo, path, i, err)
		}
		src := Source{Repo: repo, Path: path, Line: node.Line}

		var env document
		if err := sigyaml.Unmarshal(raw, &env); err != nil {
			return fmt.Errorf("%s: %w", src, err)
		}
		if env.APIVersion == "" && env.Kind == "" {
			continue // not one of ours; ignore quietly
		}
		if env.Name == "" {
			return fmt.Errorf("%s: kind %s has no metadata.name", src, env.Kind)
		}
		if err := ix.add(env, raw, src); err != nil {
			return err
		}
	}
}

func (ix *Index) add(env document, raw []byte, src Source) error {
	switch env.APIVersion {
	case APIVersion:
		return ix.addOwn(env, raw, src)
	case CoreAPIVersion:
		return ix.addCore(env, raw, src)
	default:
		return fmt.Errorf("%s: apiVersion %q is not supported (want %s or %s)", src, env.APIVersion, APIVersion, CoreAPIVersion)
	}
}

func (ix *Index) addOwn(env document, raw []byte, src Source) error {
	name := env.Name
	switch env.Kind {
	case KindApplication:
		var doc struct {
			metav1.TypeMeta   `json:",inline"`
			metav1.ObjectMeta `json:"metadata"`
			Spec              AppSpec `json:"spec"`
		}
		if err := strictDecode(raw, &doc, src, env.Kind); err != nil {
			return err
		}
		if prev, ok := ix.Applications[name]; ok {
			return duplicateErr(env.Kind, name, prev.Source, src)
		}
		if prev, ok := ix.Pods[name]; ok {
			return fmt.Errorf("%q is both an Application (%s) and a Pod (%s); a host lists both under applications, so the name must be unique", name, src, prev.Source)
		}
		ix.Applications[name] = ApplicationDoc{Metadata: doc.ObjectMeta, Spec: doc.Spec, Source: src}

	case KindGroup:
		var doc struct {
			metav1.TypeMeta   `json:",inline"`
			metav1.ObjectMeta `json:"metadata"`
			Spec              GroupSpec `json:"spec"`
		}
		if err := strictDecode(raw, &doc, src, env.Kind); err != nil {
			return err
		}
		if prev, ok := ix.Groups[name]; ok {
			return duplicateErr(env.Kind, name, prev.Source, src)
		}
		ix.Groups[name] = GroupDoc{Metadata: doc.ObjectMeta, Spec: doc.Spec, Source: src}

	case KindEnvironment:
		var doc struct {
			metav1.TypeMeta   `json:",inline"`
			metav1.ObjectMeta `json:"metadata"`
			Spec              EnvironmentSpec `json:"spec"`
		}
		if err := strictDecode(raw, &doc, src, env.Kind); err != nil {
			return err
		}
		if prev, ok := ix.Environments[name]; ok {
			return duplicateErr(env.Kind, name, prev.Source, src)
		}
		ix.Environments[name] = EnvironmentDoc{Metadata: doc.ObjectMeta, Spec: doc.Spec, Source: src}

	case KindHost:
		var doc struct {
			metav1.TypeMeta   `json:",inline"`
			metav1.ObjectMeta `json:"metadata"`
			Spec              HostSpec `json:"spec"`
		}
		if err := strictDecode(raw, &doc, src, env.Kind); err != nil {
			return err
		}
		if prev, ok := ix.Hosts[name]; ok {
			return duplicateErr(env.Kind, name, prev.Source, src)
		}
		ix.Hosts[name] = HostDoc{Metadata: doc.ObjectMeta, Spec: doc.Spec, Source: src}

	default:
		return fmt.Errorf("%s: unknown kind %q in %s", src, env.Kind, APIVersion)
	}
	return nil
}

func (ix *Index) addCore(env document, raw []byte, src Source) error {
	name := env.Name
	switch env.Kind {
	case KindPod:
		var pod corev1.Pod
		if err := strictDecode(raw, &pod, src, env.Kind); err != nil {
			return err
		}
		if prev, ok := ix.Pods[name]; ok {
			return duplicateErr(env.Kind, name, prev.Source, src)
		}
		if prev, ok := ix.Applications[name]; ok {
			return fmt.Errorf("%q is both a Pod (%s) and an Application (%s); a host lists both under applications, so the name must be unique", name, src, prev.Source)
		}
		ix.Pods[name] = PodDoc{Pod: pod, Source: src}

	case KindConfigMap:
		var cm corev1.ConfigMap
		if err := strictDecode(raw, &cm, src, env.Kind); err != nil {
			return err
		}
		if prev, ok := ix.ConfigMaps[name]; ok {
			return duplicateErr(env.Kind, name, prev.Source, src)
		}
		ix.ConfigMaps[name] = ConfigMapDoc{ConfigMap: cm, Source: src}

	case KindSecret:
		var sec corev1.Secret
		if err := strictDecode(raw, &sec, src, env.Kind); err != nil {
			return err
		}
		if err := checkSecretIsReferenceOnly(sec, src); err != nil {
			return err
		}
		if prev, ok := ix.Secrets[name]; ok {
			return duplicateErr(env.Kind, name, prev.Source, src)
		}
		ix.Secrets[name] = SecretDoc{Secret: sec, Source: src}

	default:
		return fmt.Errorf("%s: kind %q is not supported from apiVersion v1 (Pod, ConfigMap, Secret are)", src, env.Kind)
	}
	return nil
}

// checkSecretIsReferenceOnly enforces the one rule that does not bend: a
// Secret in Git carries references to values, never the values.
//
// `data` is base64 of plaintext and is refused outright. `stringData` values
// must look like "scheme:locator"; they are resolved on the host at reconcile
// time by the secrets provider.
func checkSecretIsReferenceOnly(sec corev1.Secret, src Source) error {
	if len(sec.Data) > 0 {
		keys := make([]string, 0, len(sec.Data))
		for k := range sec.Data {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return fmt.Errorf("%s: Secret %q has plaintext values in data (%s); use stringData with references such as env:NAME or file:path",
			src, sec.Name, strings.Join(keys, ", "))
	}
	var problems []string
	for k, v := range sec.StringData {
		if !looksLikeReference(v) {
			problems = append(problems, k)
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("%s: Secret %q: stringData values must be references like env:NAME or file:path, not literals (keys: %s)",
			src, sec.Name, strings.Join(problems, ", "))
	}
	return nil
}

func looksLikeReference(v string) bool {
	scheme, locator, ok := strings.Cut(v, ":")
	if !ok || scheme == "" || locator == "" {
		return false
	}
	for _, r := range scheme {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

// strictDecode decodes a YAML document against its real Go type and rejects
// unknown fields. A misspelled key is a mistake worth failing on: silently
// ignoring `imagee:` would leave a host running the wrong thing.
func strictDecode(raw []byte, out any, src Source, kind string) error {
	if err := sigyaml.UnmarshalStrict(raw, out); err != nil {
		return fmt.Errorf("%s: kind %s: %w", src, kind, err)
	}
	return nil
}

func duplicateErr(kind, name string, first, second Source) error {
	return fmt.Errorf("%s %q is defined twice: %s and %s (names must be unique across all repositories)",
		kind, name, first, second)
}

func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// jsonOf is a small helper for error messages and tests.
func jsonOf(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return err.Error()
	}
	return string(b)
}
