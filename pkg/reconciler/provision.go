package reconciler

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/podcd/podcd/pkg/config"
	"github.com/podcd/podcd/pkg/secrets"
)

// Provisioner fetches ExternalSecret targets from their stores, on demand.
//
// Resolve asks it only for the Secret names a workload on this host references,
// so a host with no Vault-backed workload never opens a connection to Vault,
// and a broken store somewhere else in the repository is not this host's
// problem. Results are cached for the life of one reconcile: two pods sharing a
// secret cost one fetch.
type Provisioner struct {
	index   *config.Index
	agent   *secrets.Resolver
	targets map[string]config.Doc[config.ExternalSecretSpec] // target Secret name -> ExternalSecret
	cache   map[string]corev1.Secret
}

// NewProvisioner indexes the ExternalSecrets by the Secret name each produces.
// agent resolves the env:/file: references inside a SecretStore's own credentials.
func NewProvisioner(index *config.Index, agent *secrets.Resolver) *Provisioner {
	targets := make(map[string]config.Doc[config.ExternalSecretSpec], len(index.ExternalSecrets))
	for name, doc := range index.ExternalSecrets {
		target := doc.Spec.Target.Name
		if target == "" {
			target = name
		}
		targets[target] = doc
	}
	return &Provisioner{index: index, agent: agent, targets: targets, cache: map[string]corev1.Secret{}}
}

// ProvisionSecret implements config.SecretProvisioner.
func (p *Provisioner) ProvisionSecret(ctx context.Context, name string) (corev1.Secret, bool, error) {
	if sec, ok := p.cache[name]; ok {
		return sec, true, nil
	}
	doc, ok := p.targets[name]
	if !ok {
		return corev1.Secret{}, false, nil
	}
	es := doc.Spec

	storeDoc, ok := p.index.SecretStores[es.SecretStoreRef.Name]
	if !ok {
		return corev1.Secret{}, false, fmt.Errorf("references unknown SecretStore %q", es.SecretStoreRef.Name)
	}
	fetcher, err := newSecretFetcher(storeDoc.Spec.Provider, p.agent)
	if err != nil {
		return corev1.Secret{}, false, err
	}

	data := make(map[string][]byte)
	for _, d := range es.Data {
		val, err := fetcher.fetch(ctx, d.RemoteRef.Key, d.RemoteRef.Property)
		if err != nil {
			return corev1.Secret{}, false, fmt.Errorf("key %q: %w", d.SecretKey, err)
		}
		data[d.SecretKey] = []byte(val)
	}
	for _, df := range es.DataFrom {
		all, err := fetcher.fetchAll(ctx, df.Extract.Key)
		if err != nil {
			return corev1.Secret{}, false, fmt.Errorf("dataFrom %q: %w", df.Extract.Key, err)
		}
		for k, v := range all {
			data[k] = []byte(v)
		}
	}

	sec := corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: config.CoreAPIVersion, Kind: config.KindSecret},
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{config.LabelManaged: "true"}},
		Data:       data,
	}
	p.cache[name] = sec
	return sec, true, nil
}

// secretFetcher abstracts one SecretStore backend's read operations.
type secretFetcher interface {
	// fetch returns the value at key (with optional property sub-key for map-valued backends like Vault).
	fetch(ctx context.Context, key, property string) (string, error)
	// fetchAll returns all key/value pairs under key (for dataFrom).
	fetchAll(ctx context.Context, key string) (map[string]string, error)
}

func newSecretFetcher(p config.SecretStoreProvider, resolver *secrets.Resolver) (secretFetcher, error) {
	switch {
	case p.Vault != nil:
		return newVaultSecretFetcher(p.Vault, resolver)
	case p.Env != nil:
		return &envSecretFetcher{resolver: resolver}, nil
	case p.File != nil:
		return &fileSecretFetcher{resolver: resolver, dir: p.File.Dir}, nil
	default:
		return nil, fmt.Errorf("SecretStore has no recognized provider (vault, env, or file)")
	}
}

// vaultSecretFetcher wraps VaultProvider for the provision phase.
type vaultSecretFetcher struct {
	p *secrets.VaultProvider
}

func newVaultSecretFetcher(spec *config.VaultStoreProvider, resolver *secrets.Resolver) (*vaultSecretFetcher, error) {
	opts := secrets.VaultOptions{
		Address:   spec.Server,
		Namespace: spec.Namespace,
		CACert:    spec.CACert,
		KVVersion: spec.KVVersion,
	}
	switch {
	case spec.Auth.AppRole != nil:
		ar := spec.Auth.AppRole
		opts.RoleID = func(ctx context.Context) (string, error) { return resolver.Resolve(ctx, ar.RoleID) }
		opts.SecretID = func(ctx context.Context) (string, error) { return resolver.Resolve(ctx, ar.SecretRef.Name) }
	case spec.Auth.TokenSecretRef != nil:
		t := spec.Auth.TokenSecretRef
		opts.Token = func(ctx context.Context) (string, error) { return resolver.Resolve(ctx, t.Name) }
	default:
		return nil, fmt.Errorf("vault: no auth method configured (want appRole or tokenSecretRef)")
	}
	vp, err := secrets.NewVaultProvider(opts)
	if err != nil {
		return nil, err
	}
	return &vaultSecretFetcher{p: vp}, nil
}

// fetch returns a single field from a Vault KV secret.
// key is the KV path including mount (e.g. "secret/prod/db"),
// property is the field name within that secret (e.g. "password").
func (v *vaultSecretFetcher) fetch(ctx context.Context, key, property string) (string, error) {
	if property == "" {
		return "", fmt.Errorf("vault: remoteRef for key %q must set property (the field within the KV secret)", key)
	}
	return v.p.Resolve(ctx, key+"/"+property)
}

// fetchAll extracts every field from a Vault KV secret at key (mount/path).
func (v *vaultSecretFetcher) fetchAll(ctx context.Context, key string) (map[string]string, error) {
	return v.p.ReadPath(ctx, key)
}

// envSecretFetcher resolves env: references via the agent resolver.
type envSecretFetcher struct {
	resolver *secrets.Resolver
}

func (e *envSecretFetcher) fetch(ctx context.Context, key, _ string) (string, error) {
	return e.resolver.Resolve(ctx, "env:"+key)
}

// fetchAll for the env store: key is a path to a KEY=VALUE file.
func (e *envSecretFetcher) fetchAll(ctx context.Context, key string) (map[string]string, error) {
	val, err := e.resolver.Resolve(ctx, "file:"+key)
	if err != nil {
		return nil, err
	}
	return parseKVPairs(val), nil
}

// fileSecretFetcher reads secrets from files under an optional root directory.
type fileSecretFetcher struct {
	resolver *secrets.Resolver
	dir      string
}

func (f *fileSecretFetcher) fetch(ctx context.Context, key, _ string) (string, error) {
	locator := key
	if f.dir != "" && !strings.HasPrefix(key, "/") {
		locator = f.dir + "/" + key
	}
	return f.resolver.Resolve(ctx, "file:"+locator)
}

func (f *fileSecretFetcher) fetchAll(_ context.Context, key string) (map[string]string, error) {
	dir := key
	if f.dir != "" && !strings.HasPrefix(key, "/") {
		dir = f.dir + "/" + key
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("file: reading directory %s: %w", dir, err)
	}
	out := make(map[string]string, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("file: reading %s: %w", entry.Name(), err)
		}
		out[entry.Name()] = strings.TrimRight(string(data), "\r\n")
	}
	return out, nil
}

// parseKVPairs splits a KEY=VALUE text into a map, skipping blank lines and comments.
func parseKVPairs(s string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}
