package secrets

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Consider using https://github.com/hashicorp/vault-client-go once it is out of beta.

// VaultOptions configures a VaultProvider.
type VaultOptions struct {
	Address   string
	Namespace string
	CACert    string // path to a PEM bundle for a private CA

	// AppRole login: both must be set, unless Token is.
	RoleID    func(context.Context) (string, error)
	SecretID  func(context.Context) (string, error)
	AuthMount string // default "approle"

	// Token is an alternative to AppRole.
	Token func(context.Context) (string, error)

	KVVersion int           // 1 or 2; default 2
	CacheTTL  time.Duration // default 30s

	HTTPClient *http.Client
}

// VaultProvider resolves vault: references against a HashiCorp Vault KV engine, authenticating with AppRole or a token.
// See ParseVaultRef for the reference syntax.
type VaultProvider struct {
	opts   VaultOptions
	client *http.Client

	mu          sync.Mutex
	token       string
	tokenExpiry time.Time
	cache       map[string]cacheEntry
}

type cacheEntry struct {
	data    map[string]string
	expires time.Time
}

// NewVaultProvider builds a provider.
func NewVaultProvider(opts VaultOptions) (*VaultProvider, error) {
	if opts.Address == "" {
		return nil, errors.New("vault: address is required")
	}
	if opts.Token == nil && (opts.RoleID == nil || opts.SecretID == nil) {
		return nil, errors.New("vault: needs roleId and secretId (AppRole) or a token")
	}
	if opts.AuthMount == "" {
		opts.AuthMount = "approle"
	}
	if opts.KVVersion == 0 {
		opts.KVVersion = 2
	}
	if opts.CacheTTL == 0 {
		opts.CacheTTL = 30 * time.Second
	}
	opts.Address = strings.TrimRight(opts.Address, "/")

	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
		if opts.CACert != "" {
			pem, err := os.ReadFile(opts.CACert)
			if err != nil {
				return nil, fmt.Errorf("vault: reading caCert: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("vault: %s holds no certificates", opts.CACert)
			}
			client.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
		}
	}
	return &VaultProvider{opts: opts, client: client, cache: map[string]cacheEntry{}}, nil
}

// Scheme implements Provider.
func (*VaultProvider) Scheme() string { return "vault" }

// VaultRef is a parsed vault: reference: which KV mount, which secret under it, and which key of that secret.
type VaultRef struct {
	Mount string
	Path  string
	Key   string
}

// String renders the mount/path/key form.
func (r VaultRef) String() string { return r.Mount + "/" + r.Path + "/" + r.Key }

// ParseVaultRef reads the locator of a vault: reference.
// Two spellings are accepted, and both of the following name the same value:
//
//	secret/prod/api/DATABASE_PASSWORD     mount first, key last, as in `vault kv get`
//	prod/api/DATABASE_PASSWORD@secret     mount after @, key last
//
// The last path segment is always the key.
// Without "@mount", the first segment is always the mount, the remainder is the secret's path.
// The rules are fixed rather than searched.
// There is deliberately no default mount, that would make "secret/prod/api/KEY" mean two different things depending on configuration.
func ParseVaultRef(locator string) (VaultRef, error) {
	const usage = "use mount/path/key or path/key@mount"
	var ref VaultRef
	rest := locator
	if before, mount, ok := strings.Cut(rest, "@"); ok {
		ref.Mount, rest = mount, before
	}
	rest = strings.Trim(rest, "/")

	i := strings.LastIndex(rest, "/")
	if i < 0 {
		return ref, fmt.Errorf("vault reference %q has no key: %s", locator, usage)
	}
	ref.Key, rest = rest[i+1:], rest[:i]

	if ref.Mount == "" {
		mount, path, ok := strings.Cut(rest, "/")
		if !ok {
			return ref, fmt.Errorf("vault reference %q has no path between mount and key: %s", locator, usage)
		}
		ref.Mount, rest = mount, path
	}
	ref.Path = strings.Trim(rest, "/")
	if ref.Mount == "" || ref.Path == "" || ref.Key == "" || strings.ContainsAny(ref.Mount+ref.Key, "/@") {
		return ref, fmt.Errorf("vault reference %q is malformed: %s", locator, usage)
	}
	return ref, nil
}

// Resolve implements Provider.
func (v *VaultProvider) Resolve(ctx context.Context, locator string) (string, error) {
	ref, err := ParseVaultRef(locator)
	if err != nil {
		return "", err
	}
	data, err := v.read(ctx, ref)
	if err != nil {
		return "", err
	}
	value, ok := data[ref.Key]
	if !ok {
		return "", fmt.Errorf("vault: %s/%s has no key %q: %w", ref.Mount, ref.Path, ref.Key, ErrNotFound)
	}
	return value, nil
}

// read returns the key/value map of one secret, from cache when fresh.
func (v *VaultProvider) read(ctx context.Context, ref VaultRef) (map[string]string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	cacheKey := ref.Mount + "/" + ref.Path
	if e, ok := v.cache[cacheKey]; ok && time.Now().Before(e.expires) {
		return e.data, nil
	}

	data, status, err := v.readOnce(ctx, ref)
	if status == http.StatusForbidden {
		// The token expired or was revoked underneath us: log in again, once.
		v.token = ""
		data, status, err = v.readOnce(ctx, ref)
	}
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, fmt.Errorf("vault: %s: %w", cacheKey, ErrNotFound)
	default:
		return nil, fmt.Errorf("vault: reading %s: HTTP %d", cacheKey, status)
	}
	v.cache[cacheKey] = cacheEntry{data: data, expires: time.Now().Add(v.opts.CacheTTL)}
	return data, nil
}

// readOnce performs one authenticated read. The caller holds the mutex.
func (v *VaultProvider) readOnce(ctx context.Context, ref VaultRef) (map[string]string, int, error) {
	if err := v.ensureToken(ctx); err != nil {
		return nil, 0, err
	}
	path := ref.Mount + "/" + ref.Path
	url := v.opts.Address + "/v1/" + ref.Mount + "/" + ref.Path
	if v.opts.KVVersion == 2 {
		url = v.opts.Address + "/v1/" + ref.Mount + "/data/" + ref.Path
	}
	body, status, err := v.do(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("vault: reading %s: %w", path, err)
	}
	if status != http.StatusOK {
		return nil, status, nil
	}

	// KV v2 nests the values one level deeper than v1.
	var parsed struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, status, fmt.Errorf("vault: parsing %s: %w", path, err)
	}
	raw := parsed.Data
	if v.opts.KVVersion == 2 {
		var inner struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(raw, &inner); err != nil {
			return nil, status, fmt.Errorf("vault: parsing %s: %w", path, err)
		}
		raw = inner.Data
	}
	var values map[string]any
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, status, fmt.Errorf("vault: parsing %s: %w", path, err)
	}
	data := make(map[string]string, len(values))
	for k, val := range values {
		switch t := val.(type) {
		case string:
			data[k] = t
		default:
			// Numbers, booleans: keep the JSON form rather than refusing.
			b, _ := json.Marshal(t)
			data[k] = string(b)
		}
	}
	return data, status, nil
}

// ensureToken logs in if there is no token or it is about to expire.
func (v *VaultProvider) ensureToken(ctx context.Context) error {
	if v.token != "" && (v.tokenExpiry.IsZero() || time.Now().Before(v.tokenExpiry)) {
		return nil
	}
	if v.opts.Token != nil {
		t, err := v.opts.Token(ctx)
		if err != nil {
			return fmt.Errorf("vault: token: %w", err)
		}
		v.token, v.tokenExpiry = t, time.Time{}
		return nil
	}

	roleID, err := v.opts.RoleID(ctx)
	if err != nil {
		return fmt.Errorf("vault: roleId: %w", err)
	}
	secretID, err := v.opts.SecretID(ctx)
	if err != nil {
		return fmt.Errorf("vault: secretId: %w", err)
	}
	payload, _ := json.Marshal(map[string]string{"role_id": roleID, "secret_id": secretID})
	v.token = "" // do not send a stale token with the login
	body, status, err := v.do(ctx, http.MethodPost, v.opts.Address+"/v1/auth/"+v.opts.AuthMount+"/login", payload)
	if err != nil {
		return fmt.Errorf("vault: approle login: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("vault: approle login: HTTP %d", status)
	}
	var login struct {
		Auth struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int    `json:"lease_duration"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(body, &login); err != nil || login.Auth.ClientToken == "" {
		return errors.New("vault: approle login returned no token")
	}
	v.token = login.Auth.ClientToken
	v.tokenExpiry = time.Time{}
	if login.Auth.LeaseDuration > 0 {
		// Renew a little early rather than racing the expiry.
		ttl := time.Duration(login.Auth.LeaseDuration) * time.Second
		v.tokenExpiry = time.Now().Add(ttl - ttl/10)
	}
	return nil
}

func (v *VaultProvider) do(ctx context.Context, method, url string, payload []byte) ([]byte, int, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, 0, err
	}
	if v.token != "" {
		req.Header.Set("X-Vault-Token", v.token)
	}
	if v.opts.Namespace != "" {
		req.Header.Set("X-Vault-Namespace", v.opts.Namespace)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return out, resp.StatusCode, nil
}
