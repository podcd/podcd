package reconciler

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	sigyaml "sigs.k8s.io/yaml"

	"github.com/podcd/podcd/pkg/config"
	"github.com/podcd/podcd/pkg/secrets"
)

// Provision resolves every ExternalSecret in index into concrete corev1.Secrets.
//
// Each resolved secret is written to <stateDir>/secrets/<name>.yaml (0600) so
// a host restart does not require a Vault round-trip before the first reconcile.
// agentResolver resolves env:/file: references inside SecretStore auth fields.
//
// Returns nil, nil when the index contains no ExternalSecrets.
func Provision(ctx context.Context, index *config.Index, stateDir string, agentResolver *secrets.Resolver) (map[string]corev1.Secret, error) {
	if len(index.ExternalSecrets) == 0 {
		return nil, nil
	}
	secretsDir := filepath.Join(stateDir, "secrets")
	if err := os.MkdirAll(secretsDir, 0o700); err != nil {
		return nil, fmt.Errorf("provision: creating secrets dir: %w", err)
	}

	out := make(map[string]corev1.Secret, len(index.ExternalSecrets))
	for name, esDoc := range index.ExternalSecrets {
		es := esDoc.Spec
		targetName := es.Target.Name
		if targetName == "" {
			targetName = name
		}

		storeDoc, ok := index.SecretStores[es.SecretStoreRef.Name]
		if !ok {
			return nil, fmt.Errorf("provision: ExternalSecret %q references unknown SecretStore %q", name, es.SecretStoreRef.Name)
		}

		fb, err := newSecretFetcher(storeDoc.Spec.Provider, agentResolver)
		if err != nil {
			return nil, fmt.Errorf("provision: ExternalSecret %q: %w", name, err)
		}

		data := make(map[string][]byte)
		for _, d := range es.Data {
			val, err := fb.fetch(ctx, d.RemoteRef.Key, d.RemoteRef.Property)
			if err != nil {
				return nil, fmt.Errorf("provision: ExternalSecret %q key %q: %w", name, d.SecretKey, err)
			}
			data[d.SecretKey] = []byte(val)
		}
		for _, df := range es.DataFrom {
			all, err := fb.fetchAll(ctx, df.Extract.Key)
			if err != nil {
				return nil, fmt.Errorf("provision: ExternalSecret %q dataFrom %q: %w", name, df.Extract.Key, err)
			}
			for k, v := range all {
				data[k] = []byte(v)
			}
		}

		sec := corev1.Secret{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
			ObjectMeta: metav1.ObjectMeta{Name: targetName, Labels: map[string]string{"io.podcd.managed": "true"}},
			Data:       data,
		}
		out[targetName] = sec

		raw, err := sigyaml.Marshal(sec)
		if err != nil {
			return nil, fmt.Errorf("provision: marshalling %s: %w", targetName, err)
		}
		if err := os.WriteFile(filepath.Join(secretsDir, targetName+".yaml"), raw, 0o600); err != nil {
			return nil, fmt.Errorf("provision: writing %s: %w", targetName, err)
		}
	}
	return out, nil
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
