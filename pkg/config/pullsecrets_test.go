package config

import (
	"context"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/podcd/podcd/pkg/model"
)

func TestPullLoginsReadsDockerConfigJSONOrUsernameAndPassword(t *testing.T) {
	// "auth" is base64("robot:pw2"); index.docker.io is what docker login writes for Docker Hub.
	cfg := corev1.Secret{StringData: map[string]string{DockerConfigJSONKey: `{"auths":{
		"registry.example.com:5000":{"username":"robot","password":"pw1"},
		"https://index.docker.io/v1/":{"auth":"cm9ib3Q6cHcy"}}}`}}
	got, err := PullLogins(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []model.RegistryLogin{
		{Server: "docker.io", Username: "robot", Password: "pw2"},
		{Server: "registry.example.com:5000", Username: "robot", Password: "pw1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}

	basic := corev1.Secret{Data: map[string][]byte{"username": []byte("u"), "password": []byte("p")}}
	got, err = PullLogins(basic, []string{"registry.example.com"})
	if err != nil || len(got) != 1 || got[0] != (model.RegistryLogin{Server: "registry.example.com", Username: "u", Password: "p"}) {
		t.Fatalf("got %+v, %v", got, err)
	}
	// Without a server in the Secret, guessing among several registries would send the password to the wrong one.
	for _, hosts := range [][]string{{"a.example.com", "b.example.com"}, {""}} {
		if _, err := PullLogins(basic, hosts); err == nil || !strings.Contains(err.Error(), "exactly one fully qualified registry") {
			t.Errorf("hosts %q: want a refusal, got %v", hosts, err)
		}
	}
	// Servers reach the login command line: one that parses as a flag must never get there.
	evil := corev1.Secret{StringData: map[string]string{DockerConfigJSONKey: `{"auths":{"--authfile=/home/u/.bashrc":{"username":"u","password":"p"}}}`}}
	if _, err := PullLogins(evil, nil); err == nil || !strings.Contains(err.Error(), "must be a host[:port]") {
		t.Errorf("a flag-shaped server must be refused, got %v", err)
	}
	if _, err := PullLogins(basic, []string{"-x.example.com"}); err == nil {
		t.Error("a flag-shaped image host must be refused")
	}
	if _, err := PullLogins(corev1.Secret{Data: map[string][]byte{"token": []byte("x")}}, []string{"r.example.com"}); err == nil {
		t.Error("a Secret with neither form must be refused")
	}
}

func TestRegistryHost(t *testing.T) {
	for image, want := range map[string]string{
		"registry.example.com/team/app:1": "registry.example.com",
		"localhost/app":                   "localhost",
		"registry:5000/app":               "registry:5000",
		"library/nginx":                   "",
		"nginx":                           "",
	} {
		if got := RegistryHost(image); got != want {
			t.Errorf("RegistryHost(%q) = %q, want %q", image, got, want)
		}
	}
}

func TestPodImagePullSecretBecomesALoginAndIsNotPlayed(t *testing.T) {
	ix := NewIndex()
	if err := ix.LoadTree("test", writeTree(t, map[string]string{"pod.yaml": `
apiVersion: v1
kind: Secret
metadata: {name: pull}
stringData: {username: robot, password: s3cret}
---
apiVersion: v1
kind: Pod
metadata: {name: api}
spec:
  imagePullSecrets: [{name: pull}]
  containers:
    - name: api
      image: registry.example.com/api` + digest + `
---
apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: vm-1}
spec: {applications: [api]}
`})); err != nil {
		t.Fatal(err)
	}
	got, err := ix.Resolve(context.Background(), ResolveOptions{Host: "vm-1"})
	if err != nil {
		t.Fatal(err)
	}
	app := got.Applications[0]
	if len(app.Logins) != 1 || app.Logins[0] != (model.RegistryLogin{Server: "registry.example.com", Username: "robot", Password: "s3cret"}) {
		t.Fatalf("logins %+v", app.Logins)
	}
	if m := string(app.Manifest); strings.Contains(m, "imagePullSecrets") || strings.Contains(m, "s3cret") {
		t.Fatalf("the pull Secret must not be played:\n%s", m)
	}
}
