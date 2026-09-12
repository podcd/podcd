package config

import (
	"bufio"
	"bytes"
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
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	sigyaml "sigs.k8s.io/yaml"
)

// Index is every document the agent loaded, addressed by name and kind.
//
// Names are global across repositories on purpose.
// Two repositories defining the same Application is an ambiguity, and ambiguity is an error here, not a coin flip.
// Applications and Pods share one namespace too, because a host lists both under `applications:`.
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
	switch env.APIVersion {
	case APIVersion:
		return ix.addOwn(env, src)
	case CoreAPIVersion:
		return ix.addCore(env, src)
	default:
		return fmt.Errorf("%s: apiVersion %q is not supported (want %s or %s)", src, env.APIVersion, APIVersion, CoreAPIVersion)
	}
}

func (ix *Index) addOwn(env document, src Source) error {
	name := env.Name
	switch env.Kind {
	case KindApplication:
		var doc struct {
			metav1.TypeMeta   `json:",inline"`
			metav1.ObjectMeta `json:"metadata"`
			Spec              AppSpec `json:"spec"`
		}
		if err := strictDecode(&doc, src, env.Kind); err != nil {
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
		if err := strictDecode(&doc, src, env.Kind); err != nil {
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
		if err := strictDecode(&doc, src, env.Kind); err != nil {
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
		if err := strictDecode(&doc, src, env.Kind); err != nil {
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

func (ix *Index) addCore(env document, src Source) error {
	name := env.Name
	switch env.Kind {
	case KindPod:
		var pod corev1.Pod
		if err := strictDecode(&pod, src, env.Kind); err != nil {
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
		if err := strictDecode(&cm, src, env.Kind); err != nil {
			return err
		}
		if prev, ok := ix.ConfigMaps[name]; ok {
			return duplicateErr(env.Kind, name, prev.Source, src)
		}
		ix.ConfigMaps[name] = ConfigMapDoc{ConfigMap: cm, Source: src}

	case KindSecret:
		var sec corev1.Secret
		if err := strictDecode(&sec, src, env.Kind); err != nil {
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

// checkSecretIsReferenceOnly enforces the one rule that does not bend: a Secret in Git carries references to values, never the values.
//
// `data` is base64 of plaintext and is refused outright.
// `stringData` values must look like "scheme:locator", they are resolved on the host at reconcile time by the secrets provider.
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

// strictDecode decodes a YAML document against its real Go type and rejects unknown fields.
// A misspelled key is a mistake worth failing on: silently ignoring `imagee:` would leave a host running the wrong thing.
func strictDecode(out any, src Source, kind string) error {
	if err := sigyaml.UnmarshalStrict(src.Raw, out); err != nil {
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
