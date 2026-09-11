// Package secrets resolves secret references found in Git into values that
// never were in Git.
//
// A reference looks like "scheme:locator", e.g. "env:API_TOKEN" or
// "file:/etc/podcd/secrets/api-token". The MVP ships env- and file-backed
// providers; Vault, KMS, SOPS and friends implement the same interface later.
package secrets

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

// Default returns the resolver used when nothing else is configured:
// environment variables and files readable by the agent user.
func Default(secretsDir string, envFiles ...string) *Resolver {
	explicit := ""
	if len(envFiles) > 0 {
		explicit = envFiles[0]
	}
	return NewResolver(EnvProvider{Paths: envFilePaths(explicit)}, FileProvider{Root: secretsDir})
}

// Schemes lists the registered schemes, sorted.
func (r *Resolver) Schemes() []string {
	out := make([]string, 0, len(r.providers))
	for s := range r.providers {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Resolve looks up one "scheme:locator" reference.
//
// A reference with no scheme is rejected rather than treated as a literal: a
// plaintext secret in Git must never work by accident.
func (r *Resolver) Resolve(ctx context.Context, ref string) (string, error) {
	scheme, locator, ok := strings.Cut(ref, ":")
	if !ok || scheme == "" || locator == "" {
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

// EnvProvider reads secrets from the agent's own environment, typically
// supplied by a systemd EnvironmentFile that is not in Git.
type EnvProvider struct {
	Paths []string
}

// Scheme implements Provider.
func (EnvProvider) Scheme() string { return "env" }

// Resolve implements Provider.
func (p EnvProvider) Resolve(_ context.Context, locator string) (string, error) {
	if v, ok := os.LookupEnv(locator); ok {
		return v, nil
	}
	paths := p.Paths
	if len(paths) == 0 {
		paths = envFilePaths("")
	}
	if v, ok := lookupEnvFile(locator, paths...); ok {
		return v, nil
	}
	return "", fmt.Errorf("environment variable %s is not set: %w", locator, ErrNotFound)
}

func envFilePaths(configured string) []string {
	paths := []string{}
	if envFile := os.Getenv("PODCD_ENV_FILE"); envFile != "" {
		paths = append(paths, envFile)
	}
	if configured != "" {
		paths = append(paths, configured)
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".config", "podcd", "agent.env"))
	}
	paths = append(paths, filepath.Join("/etc", "podcd", "agent.env"))
	seen := map[string]struct{}{}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

func lookupEnvFile(locator string, paths ...string) (string, bool) {
	for _, path := range paths {
		if path == "" {
			continue
		}
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if strings.HasPrefix(line, "export ") {
				line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
			}
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			key = strings.TrimSpace(key)
			if key == locator {
				_ = file.Close()
				return strings.Trim(value, "\"'"), true
			}
		}
		_ = file.Close()
		if err := scanner.Err(); err != nil {
			break
		}
	}
	return "", false
}

// FileProvider reads secrets from files on the host. Relative locators resolve
// under Root; absolute paths are used as given.
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
