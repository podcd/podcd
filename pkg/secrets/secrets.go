// Package secrets resolves secret references found in Git into values that never were in Git.
//
// A reference looks like "scheme:locator", e.g. "env:API_TOKEN" or "file:/etc/podcd/secrets/api-token".
// The MVP ships env- and file-backed providers.
// Vault, KMS, SOPS and friends implement the same interface later.
package secrets

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
)

// ErrNotFound is returned when a reference resolves to nothing.
var ErrNotFound = errors.New("secret not found")

// Provider resolves one scheme of secret reference.
type Provider interface {
	// Scheme is the reference prefix this provider answers to, e.g. "env".
	Scheme() string
	// Resolve returns the secret value for a locator (the part after "scheme:").
	Resolve(ctx context.Context, locator string) (string, error)
}

// Resolver dispatches references to providers by scheme.
type Resolver struct {
	providers map[string]Provider
}

// NewResolver builds a resolver over the given providers.
func NewResolver(providers ...Provider) *Resolver {
	r := &Resolver{providers: map[string]Provider{}}
	for _, p := range providers {
		r.providers[p.Scheme()] = p
	}
	return r
}

// Default returns the resolver used when nothing else is configured: the agent's environment (optionally backed by one env file), and files readable by the agent user.
func Default(secretsDir, envFile string) *Resolver {
	return NewResolver(EnvProvider{File: envFile}, FileProvider{Root: secretsDir})
}

// Schemes lists the registered schemes, sorted.
func (r *Resolver) Schemes() []string { return slices.Sorted(maps.Keys(r.providers)) }

// SplitRef splits a "scheme:locator" reference. ok is false for anything else.
// A plaintext secret in Git should never work by accident.
func SplitRef(ref string) (scheme, locator string, ok bool) {
	scheme, locator, ok = strings.Cut(ref, ":")
	if !ok || scheme == "" || locator == "" {
		return "", "", false
	}
	for _, r := range scheme {
		if r < 'a' || r > 'z' {
			return "", "", false
		}
	}
	return scheme, locator, true
}

// IsReference reports whether v has the shape of a secret reference.
func IsReference(v string) bool {
	_, _, ok := SplitRef(v)
	return ok
}

// Resolve looks up one "scheme:locator" reference.
func (r *Resolver) Resolve(ctx context.Context, ref string) (string, error) {
	scheme, locator, ok := SplitRef(ref)
	if !ok {
		return "", fmt.Errorf("secret reference %q must be \"scheme:locator\" (known schemes: %s)",
			ref, strings.Join(r.Schemes(), ", "))
	}
	p, ok := r.providers[scheme]
	if !ok {
		return "", fmt.Errorf("secret reference %q: no provider for scheme %q (known schemes: %s)",
			ref, scheme, strings.Join(r.Schemes(), ", "))
	}
	v, err := p.Resolve(ctx, locator)
	if err != nil {
		return "", fmt.Errorf("secret %q: %w", ref, err)
	}
	return v, nil
}

type EnvProvider struct {
	File string
}

// Scheme implements Provider.
func (EnvProvider) Scheme() string { return "env" }

// Resolve implements Provider.
func (p EnvProvider) Resolve(_ context.Context, locator string) (string, error) {
	if p.File != "" {
		vars, err := readEnvFile(p.File)
		if err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("reading %s: %w", p.File, err)
		}
		if v, ok := vars[locator]; ok {
			return v, nil
		}
	}
	if v, ok := os.LookupEnv(locator); ok {
		return v, nil
	}
	where := "the environment"
	if p.File != "" {
		where = p.File + " or the environment"
	}
	return "", fmt.Errorf("%s is not in %s: %w", locator, where, ErrNotFound)
}

func readEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	vars := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		vars[strings.TrimSpace(key)] = unquote(strings.TrimSpace(value))
	}
	return vars, sc.Err()
}

func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}

// FileProvider reads secrets from files on the host.
// Relative locators resolve under Root, absolute paths are used as given.
type FileProvider struct {
	Root string
}

// Scheme implements Provider.
func (FileProvider) Scheme() string { return "file" }

// Resolve implements Provider.
func (f FileProvider) Resolve(_ context.Context, locator string) (string, error) {
	path := locator
	if !strings.HasPrefix(path, "/") {
		if f.Root == "" {
			return "", fmt.Errorf("relative secret path %q but no secrets directory configured", locator)
		}
		path = f.Root + "/" + locator
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%s: %w", path, ErrNotFound)
		}
		return "", err
	}
	// A trailing newline is an artifact of writing the file, not part of the secret.
	return strings.TrimRight(string(b), "\r\n"), nil
}
