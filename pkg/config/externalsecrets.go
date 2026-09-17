package config

// ExternalSecretsAPIVersion is the API version used for ExternalSecret and
// SecretStore documents. Field names are compatible with external-secrets.io/v1beta1
// so existing YAML written for that operator decodes here without modification.
// We use our own Go structs to avoid pulling in controller-runtime.
const ExternalSecretsAPIVersion = "external-secrets.io/v1beta1"

const (
	KindSecretStore    = "SecretStore"
	KindExternalSecret = "ExternalSecret"
)

// SecretStoreSpec is the body of a SecretStore document.
type SecretStoreSpec struct {
	Provider SecretStoreProvider `json:"provider"`
}

// SecretStoreProvider holds exactly one backend configuration.
type SecretStoreProvider struct {
	Vault *VaultStoreProvider `json:"vault,omitempty"`
	Env   *EnvStoreProvider   `json:"env,omitempty"`
	File  *FileStoreProvider  `json:"file,omitempty"`
}

// VaultStoreProvider configures a HashiCorp Vault KV backend.
// Vault credentials are secret references (env:NAME, file:path) resolved from
// agent.env at reconcile time - never stored as plaintext in Git.
type VaultStoreProvider struct {
	// Server is the Vault address, e.g. https://vault.example.com
	Server string `json:"server"`
	// Namespace is an optional Vault Enterprise namespace.
	Namespace string `json:"namespace,omitempty"`
	// CACert is an optional path to a PEM CA certificate.
	CACert string `json:"caCert,omitempty"`
	// KVVersion selects KV v1 or v2. Defaults to 2.
	KVVersion int       `json:"kvVersion,omitempty"`
	Auth      VaultAuth `json:"auth"`
}

// VaultAuth holds one Vault authentication method. Exactly one field must be set.
type VaultAuth struct {
	// AppRole authenticates with a role ID + secret ID pair.
	AppRole *VaultAppRoleAuth `json:"appRole,omitempty"`
	// TokenSecretRef authenticates with a static token stored as a secret reference.
	TokenSecretRef *SecretKeySelector `json:"tokenSecretRef,omitempty"`
}

// VaultAppRoleAuth carries AppRole credentials.
// Both values are secret references resolved from agent.env.
type VaultAppRoleAuth struct {
	// RoleID is a secret reference to the AppRole role ID (e.g. env:VAULT_ROLE_ID).
	RoleID string `json:"roleId"`
	// SecretRef is a secret reference to the AppRole secret ID.
	SecretRef SecretKeySelector `json:"secretRef"`
}

// SecretKeySelector names a key within a secret source.
type SecretKeySelector struct {
	// Name is a secret reference (env:NAME or file:path) or the name of a
	// previously provisioned Kubernetes Secret.
	Name string `json:"name"`
	// Key selects one key within a named Secret (unused for inline references).
	Key string `json:"key,omitempty"`
}

// EnvStoreProvider resolves values from the agent's environment (agent.env).
// No configuration is required.
type EnvStoreProvider struct{}

// FileStoreProvider resolves values from files under secretsDir.
type FileStoreProvider struct {
	// Dir overrides the global secretsDir for this store.
	Dir string `json:"dir,omitempty"`
}

// ExternalSecretSpec is the body of an ExternalSecret document.
type ExternalSecretSpec struct {
	// SecretStoreRef names the SecretStore to use.
	SecretStoreRef SecretStoreRef `json:"secretStoreRef"`
	// Target describes the Kubernetes Secret to create.
	Target ExternalSecretTarget `json:"target"`
	// Data maps individual remote keys to Secret keys.
	Data []ExternalSecretData `json:"data,omitempty"`
	// DataFrom extracts all keys from a remote path.
	DataFrom []ExternalSecretDataFrom `json:"dataFrom,omitempty"`
}

// SecretStoreRef names a SecretStore document.
type SecretStoreRef struct {
	Name string `json:"name"`
	// Kind defaults to SecretStore; ClusterSecretStore is not supported.
	Kind string `json:"kind,omitempty"`
}

// ExternalSecretTarget describes the Kubernetes Secret to produce.
type ExternalSecretTarget struct {
	// Name of the Secret to create. Defaults to the ExternalSecret name.
	Name string `json:"name,omitempty"`
}

// ExternalSecretData maps one remote key to one Secret key.
type ExternalSecretData struct {
	// SecretKey is the key written into the target Secret.
	SecretKey string                      `json:"secretKey"`
	RemoteRef ExternalSecretDataRemoteRef `json:"remoteRef"`
}

// ExternalSecretDataRemoteRef identifies a value in the remote backend.
type ExternalSecretDataRemoteRef struct {
	// Key is the path in the backend (Vault KV path, env var name, file name).
	Key string `json:"key"`
	// Property selects a sub-key within a map value (Vault KV returns maps).
	Property string `json:"property,omitempty"`
}

// ExternalSecretDataFrom extracts all key/value pairs from a remote path.
type ExternalSecretDataFrom struct {
	Extract ExternalSecretDataRemoteRef `json:"extract"`
}
