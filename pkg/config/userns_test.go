package config

import (
	"strings"
	"testing"
)

// podWithUserNS returns a Pod document carrying the userns annotation.
func podWithUserNS(name, userns string) string {
	return `
apiVersion: v1
kind: Pod
metadata:
  name: ` + name + `
  annotations:
    io.podcd.userns: "` + userns + `"
spec:
  containers:
    - name: ` + name + `
      image: example.com/` + name + digest + `
`
}

func TestUserNSAnnotationReachesTheApplication(t *testing.T) {
	got, err := resolveVM1(t, map[string]string{
		"apps/api.yaml":    podWithUserNS("api", " keep-id:uid=1002,gid=1002 "),
		"apps/worker.yaml": podOn("worker", ""),
		"host.yaml":        hostVM1,
	})
	if err != nil {
		t.Fatal(err)
	}
	api, _ := got.App("api")
	if api.UserNS != "keep-id:uid=1002,gid=1002" {
		t.Fatalf("userns = %q", api.UserNS)
	}
	worker, _ := got.App("worker")
	if worker.UserNS != "" {
		t.Fatalf("no annotation means podman's default, got %q", worker.UserNS)
	}
}

func TestUserNSAnnotationIsChecked(t *testing.T) {
	for _, tc := range []struct{ value, want string }{
		{"keepid", `unknown mode "keepid"`},
		{"keep-id uid=1", "must not contain whitespace"},
	} {
		_, err := resolveVM1(t, map[string]string{
			"apps/api.yaml":    podWithUserNS("api", tc.value),
			"apps/worker.yaml": podOn("worker", ""),
			"host.yaml":        hostVM1,
		})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%q: want an error mentioning %q, got %v", tc.value, tc.want, err)
		}
	}
	for _, ok := range []string{"auto", "auto:size=65536", "host", "keep-id", "nomap", "ns:/proc/1/ns/user", "private"} {
		if msg := checkUserNS(ok); msg != "" {
			t.Errorf("%q is a podman mode, got %q", ok, msg)
		}
	}
}
