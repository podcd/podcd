package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// SpecHash is the fingerprint of an application's desired state.
// Provenance fields are excluded: moving a YAML document between repositories
// is not a change to the running app.
func (a Application) SpecHash() string {
	payload := a
	payload.SourceRepo = ""
	payload.Origins = nil
	b, err := json.Marshal(payload)
	if err != nil {
		// Application contains only plain data; marshalling cannot fail.
		panic(fmt.Sprintf("model: hashing application %q: %v", a.Name, err))
	}
	return hashString(string(b))
}

func hashString(s string) string { return HashBytes([]byte(s)) }

// HashBytes returns the hex sha256 of b. Used for comparing rendered unit files.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
