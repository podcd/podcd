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
)

// documents is every decoded document of every kind, addressed by name.
//
// Names are global across repositories on purpose.
// Two repositories defining the same Pod is an ambiguity, and ambiguity is an error here, not a coin flip.
//
// The Index holds one set for the plain .yaml files in the tree; Resolve
// builds a second, per host, from what that host's templates render to. Both
// go through the same decode path (addDocuments), so a rendered document is
// validated exactly like a written one.
type documents struct {
	Groups       map[string]Doc[SelectionSpec]
	Environments map[string]Doc[SelectionSpec]
	Hosts        map[string]Doc[HostSpec]

	Pods       map[string]Doc[corev1.Pod]
	ConfigMaps map[string]Doc[corev1.ConfigMap]
	Secrets    map[string]Doc[corev1.Secret]

	SecretStores    map[string]Doc[SecretStoreSpec]
	ExternalSecrets map[string]Doc[ExternalSecretSpec]
}

func newDocuments() documents {
	return documents{
		Groups:          map[string]Doc[SelectionSpec]{},
		Environments:    map[string]Doc[SelectionSpec]{},
		Hosts:           map[string]Doc[HostSpec]{},
		Pods:            map[string]Doc[corev1.Pod]{},
		ConfigMaps:      map[string]Doc[corev1.ConfigMap]{},
		Secrets:         map[string]Doc[corev1.Secret]{},
		SecretStores:    map[string]Doc[SecretStoreSpec]{},
		ExternalSecrets: map[string]Doc[ExternalSecretSpec]{},
	}
}

// Index is everything the agent loaded from Git: the plain documents,
// decoded; the templates, still raw; and every file's bytes.
type Index struct {
	documents

	// templates are the *.tpl files, kept as text. A template is not a
	// document until it is rendered, and it cannot be rendered until a host
	// and that host's values are known - so nothing about it is decided
	// here. See Resolve. Which files are templates is decided by their
	// name, never by their contents: a plain .yaml may contain "{{" and it
	// is just text.
	templates []Source

	// files holds every loaded file's raw bytes, keyed by repository and the
	// path it was loaded under - not just recognized documents. A Host,
	// Group or Environment's own `values:` list is resolved against this
	// cache in Resolve, so reading it back never touches disk again.
	files map[string]map[string][]byte
}

// NewIndex returns an empty index.
func NewIndex() *Index {
	return &Index{
		documents: newDocuments(),
		files:     map[string]map[string][]byte{},
	}
}

// Templates returns the template files loaded, in load order.
func (ix *Index) Templates() []Source { return ix.templates }

// TemplateSuffix marks a file as a template rather than a document.
const TemplateSuffix = ".tpl"

// isTemplate reports whether a file name declares a template: foo.yaml.tpl
// (or any other *.tpl).
func isTemplate(name string) bool { return strings.HasSuffix(strings.ToLower(name), TemplateSuffix) }

// isDocument reports whether a file name is a plain YAML document.
func isDocument(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".yaml" || ext == ".yml"
}

// HostNames returns the known host names, sorted.
func (ix *Index) HostNames() []string { return slices.Sorted(maps.Keys(ix.Hosts)) }

// LoadTree walks one repository checkout and adds every document and template it finds.
//
// Files are visited in sorted path order, so load order never depends on the filesystem.
// Only .yaml/.yml (documents) and .tpl (templates) are read, everything else
// (READMEs, scripts, Containerfiles) is ignored. Dot-directories are skipped.
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
		if isDocument(name) || isTemplate(name) {
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

// LoadBytes adds one file's content, routed by its name the same way LoadTree
// routes files it finds: *.tpl is a template, anything else a document.
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

// loadFile routes one file by its name: a *.tpl is kept raw as a template,
// anything else is decoded as documents, strictly, here and now.
func (ix *Index) loadFile(repo, path string, data []byte) error {
	if ix.files[repo] == nil {
		ix.files[repo] = map[string][]byte{}
	}
	ix.files[repo][path] = data

	if isTemplate(path) {
		ix.templates = append(ix.templates, Source{Repo: repo, Path: path, Line: 1, Raw: data})
		return nil
	}
	return ix.addDocuments(repo, path, data, false)
}

// addDocuments decodes one possibly multi-document YAML file into d.
// Each document is decoded strictly against its real Go type. An unknown
// field is an error whichever family the document belongs to.
//
// rendered marks text that came out of a template rather than a file. A
// template may only produce deployable resources: a Host, Group or
// Environment decides which values a host gets, so it cannot itself depend
// on them - and a rendered document's line numbers refer to the rendered
// text.
func (d *documents) addDocuments(repo, path string, data []byte, rendered bool) error {
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
		if rendered && !deployable(env.Kind) {
			return fmt.Errorf("%s: a template may only render an Application, Pod, ConfigMap or Secret, not a %s: those decide which values a host gets, so they cannot depend on them", src, env.Kind)
		}
		if err := d.add(env, src); err != nil {
			return err
		}
	}
	return nil
}

// deployable reports whether a kind is something a host runs, as opposed
// to something that decides what a host runs.
func deployable(kind string) bool {
	switch kind {
	case KindPod, KindConfigMap, KindSecret:
		return true
	}
	return false
}

func (d *documents) add(env document, src Source) error {
	name := env.Name
	switch env.APIVersion {
	case APIVersion:
		switch env.Kind {
		case KindGroup:
			return addSpec(d.Groups, env.Kind, name, src)
		case KindEnvironment:
			return addSpec(d.Environments, env.Kind, name, src)
		case KindHost:
			return addSpec(d.Hosts, env.Kind, name, src)
		}
		return fmt.Errorf("%s: unknown kind %q in %s", src, env.Kind, APIVersion)
	case CoreAPIVersion:
		switch env.Kind {
		case KindPod:
			return addObject(d.Pods, env.Kind, name, src, nil)
		case KindConfigMap:
			return addObject(d.ConfigMaps, env.Kind, name, src, nil)
		case KindSecret:
			return addObject(d.Secrets, env.Kind, name, src, nil)
		}
		return fmt.Errorf("%s: kind %q is not supported from apiVersion v1 (Pod, ConfigMap, Secret are)", src, env.Kind)
	case ExternalSecretsAPIVersion:
		switch env.Kind {
		case KindSecretStore:
			return addSpec(d.SecretStores, env.Kind, name, src)
		case KindExternalSecret:
			return addSpec(d.ExternalSecrets, env.Kind, name, src)
		}
		return fmt.Errorf("%s: kind %q is not supported from apiVersion %s (SecretStore, ExternalSecret are)", src, env.Kind, ExternalSecretsAPIVersion)
	default:
		return fmt.Errorf("%s: apiVersion %q is not supported (want %s, %s, or %s)", src, env.APIVersion, APIVersion, CoreAPIVersion, ExternalSecretsAPIVersion)
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

// readValuesFile resolves one entry of a Host, Group or Environment's own
// `values:` list against the repository that document came from - the same
// repository, because there is no other document-relative anchor to resolve
// a bare path against.
func (ix *Index) readValuesFile(repo, path string) (Values, error) {
	data, ok := ix.files[repo][path]
	if !ok {
		if repo == "" {
			return nil, fmt.Errorf("values file %q was not loaded", path)
		}
		return nil, fmt.Errorf("values file %q was not loaded from repository %q", path, repo)
	}
	return parseValues(Source{Repo: repo, Path: path}.String(), data)
}

// strictDecode decodes a YAML document against its real Go type and rejects unknown fields.
// A misspelled key is a mistake worth failing on: silently ignoring `imagee:` would leave a host running the wrong thing.
func strictDecode(out any, src Source, kind string) error {
	if err := sigyaml.UnmarshalStrict(src.Raw, out); err != nil {
		return fmt.Errorf("%s: kind %s: %w", src, kind, err)
	}
	return nil
}
