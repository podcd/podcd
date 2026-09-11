// Package identity answers "which VM am I?" - the question that decides what
// this machine should run.
package identity

import (
	"fmt"
	"os"
	"strings"
)

// Identity is how this host is addressed in Git.
type Identity struct {
	// Host is the name matched against Host documents.
	Host string `json:"host"`
	// Source records how the name was found, for debugging a host that picked
	// up the wrong configuration.
	Source string `json:"source"`
	// MachineID is the systemd machine id when readable. Informational only.
	MachineID string `json:"machineId,omitempty"`
	// Hostname is the raw system hostname.
	Hostname string `json:"hostname"`
}

// Resolve determines the identity of this host.
//
// Precedence: the PODCD_HOST environment variable, then the agent config's
// host field, then the system hostname with any domain stripped. There is no
// fourth guess: if all three are empty, that is an error, because quietly
// reconciling against the wrong host's configuration is worse than stopping.
func Resolve(configured string) (Identity, error) {
	id := Identity{}
	id.Hostname = systemHostname()
	id.MachineID = machineID()

	switch {
	case os.Getenv("PODCD_HOST") != "":
		id.Host = os.Getenv("PODCD_HOST")
		id.Source = "PODCD_HOST environment variable"
	case configured != "":
		id.Host = configured
		id.Source = "agent config host field"
	case id.Hostname != "":
		id.Host = id.Hostname
		id.Source = "system hostname"
	default:
		return id, fmt.Errorf("cannot determine host identity: no PODCD_HOST, no host in the agent config, and no usable hostname")
	}
	return id, nil
}

func systemHostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	// A short name is what people put in Git; prod-web-01.example.com and
	// prod-web-01 should not be two different hosts.
	if i := strings.Index(h, "."); i > 0 {
		h = h[:i]
	}
	return strings.ToLower(strings.TrimSpace(h))
}

func machineID() string {
	for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if b, err := os.ReadFile(p); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}
