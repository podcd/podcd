package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// hashView drops Application's redacting MarshalJSON so the hash can see the
// real (already one-way hashed) secret material.
type hashView Application

// SpecHash is the fingerprint of an application's desired state.
//
// Secret values are folded in as one-way hashes, so rotating a secret changes
// the fingerprint - and therefore restarts the app - without the plaintext ever
// reaching the hash input of anything that gets written down. Provenance fields
// are excluded: moving a YAML document between repositories is not a change to
// the running app.
func (a Application) SpecHash() string {
	payload := a
	payload.SourceRepo = ""
	payload.Origins = nil
	if len(a.SecretEnv) > 0 {
		hashed := make(map[string]string, len(a.SecretEnv))
		for k, v := range a.SecretEnv {
			hashed[k] = hashString("secret:" + k + "\x00" + v)[:16]
		}
		payload.SecretEnv = hashed
	}
	b, err := json.Marshal(hashView(payload))
	if err != nil {
		// Application contains only plain data; marshalling cannot fail.
		panic(fmt.Sprintf("model: hashing application %q: %v", a.Name, err))
	}
	return hashString(string(b))
}

// SecretsHash fingerprints just the resolved secret values.
// s.t the executor could tell whether the on-disk env file needs rewriting.

func (a Application) SecretsHash() string {
	if len(a.SecretEnv) == 0 {
		return ""
	}
	keys := make([]string, 0, len(a.SecretEnv))
	for k := range a.SecretEnv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	buf := ""
	for _, k := range keys {
		buf += k + "\x00" + a.SecretEnv[k] + "\x00"
	}
	return hashString(buf)
}

func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// HashBytes returns the hex sha256 of b. Used for comparing rendered unit files.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
