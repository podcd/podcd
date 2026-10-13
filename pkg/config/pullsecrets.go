package config

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/podcd/podcd/pkg/model"
)

// DockerConfigJSONKey is the key of a kubernetes.io/dockerconfigjson Secret.
const DockerConfigJSONKey = ".dockerconfigjson"

// RegistryHost is the registry an image is pulled from, or "" for a short
// name, which podman resolves through registries.conf.
func RegistryHost(image string) string {
	first, _, ok := strings.Cut(image, "/")
	if !ok || (!strings.ContainsAny(first, ".:") && first != "localhost") {
		return ""
	}
	return first
}

// CheckRegistryServer rejects a login server that is not a plain host[:port]:
// it ends up on the login command line, and may come from Secret content.
func CheckRegistryServer(server string) error {
	switch {
	case server == "":
		return fmt.Errorf("registry server is empty")
	case strings.HasPrefix(server, "-"), strings.ContainsAny(server, "/@ \t\r\n"), strings.Contains(server, "://"):
		return fmt.Errorf("registry server %q must be a host[:port]", server)
	}
	return nil
}

// PullLogins reads registry credentials from an image pull Secret: every entry
// of a .dockerconfigjson, or username and password keys for the one registry
// in hosts.
func PullLogins(sec corev1.Secret, hosts []string) ([]model.RegistryLogin, error) {
	data := map[string]string{}
	for k, v := range sec.Data {
		data[k] = string(v)
	}
	maps.Copy(data, sec.StringData)

	if raw, ok := data[DockerConfigJSONKey]; ok {
		var cfg struct {
			Auths map[string]struct {
				Username string `json:"username"`
				Password string `json:"password"`
				Auth     string `json:"auth"`
			} `json:"auths"`
		}
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			return nil, fmt.Errorf("%s is not valid JSON: %w", DockerConfigJSONKey, err)
		}
		var logins []model.RegistryLogin
		for _, server := range slices.Sorted(maps.Keys(cfg.Auths)) {
			a := cfg.Auths[server]
			user, pass := a.Username, a.Password
			if a.Auth != "" && user == "" {
				dec, err := base64.StdEncoding.DecodeString(a.Auth)
				if err != nil {
					return nil, fmt.Errorf("%s: auth for %s is not base64", DockerConfigJSONKey, server)
				}
				user, pass, _ = strings.Cut(string(dec), ":")
			}
			if user == "" || pass == "" {
				return nil, fmt.Errorf("%s: no username and password for %s", DockerConfigJSONKey, server)
			}
			srv := dockerConfigServer(server)
			if err := CheckRegistryServer(srv); err != nil {
				return nil, fmt.Errorf("%s: %w", DockerConfigJSONKey, err)
			}
			logins = append(logins, model.RegistryLogin{Server: srv, Username: user, Password: pass})
		}
		if len(logins) == 0 {
			return nil, fmt.Errorf("%s has no auths", DockerConfigJSONKey)
		}
		return logins, nil
	}

	user, pass := data["username"], data["password"]
	if user == "" || pass == "" {
		return nil, fmt.Errorf("needs a %s key, or username and password keys", DockerConfigJSONKey)
	}
	if len(hosts) != 1 || hosts[0] == "" {
		return nil, fmt.Errorf("username and password name no registry, and the images do not come from exactly one fully qualified registry (%s); use a %s key",
			strings.Join(hosts, ", "), DockerConfigJSONKey)
	}
	if err := CheckRegistryServer(hosts[0]); err != nil {
		return nil, err
	}
	return []model.RegistryLogin{{Server: hosts[0], Username: user, Password: pass}}, nil
}

// imageHosts lists the distinct registries of images, "" standing for any short name.
func imageHosts(images []string) []string {
	seen := map[string]bool{}
	for _, img := range images {
		seen[RegistryHost(img)] = true
	}
	return slices.Sorted(maps.Keys(seen))
}

// dockerConfigServer turns a docker config key ("https://index.docker.io/v1/") into a login server.
func dockerConfigServer(key string) string {
	key = strings.TrimPrefix(strings.TrimPrefix(key, "https://"), "http://")
	key, _, _ = strings.Cut(key, "/")
	if key == "index.docker.io" {
		return "docker.io"
	}
	return key
}
