package config

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/podcd/podcd/pkg/model"
)

func resolveOne(t *testing.T, files map[string]string) (model.Application, error) {
	t.Helper()
	ix := loadIndex(t, files)
	got, err := ix.Resolve(context.Background(), ResolveOptions{Host: "prod-web-01"})
	if err != nil {
		return model.Application{}, err
	}
	for _, a := range got.Applications {
		if a.Name == "api" {
			return a, nil
		}
	}
	t.Fatalf("api not in %v", got.Names())
	return model.Application{}, nil
}

func TestSlicesReplaceAndMapsMergeAcrossLayers(t *testing.T) {
	files := baseFiles()
	files["apps/api.yaml"] = `
apiVersion: v1
kind: Pod
metadata:
  name: api
  labels: {team: core}
  annotations:
    io.podcd.networks: "a"
spec:
  containers:
    - name: api
      image: example.com/api` + digest + `
      ports:
        - containerPort: 8080
          hostPort: 8080
      env:
        - name: A
          value: base
`
	files["groups/web.yaml"] = `
apiVersion: gitops.podcd.io/v1
kind: Group
metadata:
  name: web
spec:
  applications: [api, frontend]
  overrides:
    api:
      metadata:
        labels: {tier: web}
      spec:
        containers:
          - name: api
            ports:
              - containerPort: 8080
                hostPort: 8082
            env:
              - name: B
                value: group
`
	app, err := resolveOne(t, files)
	if err != nil {
		t.Fatal(err)
	}
	if len(app.Ports) != 1 || app.Ports[0].Host != 8082 {
		t.Errorf("a port list in an override replaces the base list, got %+v", app.Ports)
	}
	if app.Env["A"] != "base" || app.Env["B"] != "group" {
		t.Errorf("env merges key by key with the higher layer winning, got %v", app.Env)
	}
	if app.Labels["team"] != "core" || app.Labels["tier"] != "web" {
		t.Errorf("labels merge, got %v", app.Labels)
	}
	if strings.Join(app.Networks, ",") != "a" {
		t.Errorf("an absent slice leaves the base alone, got %v", app.Networks)
	}
}

func TestPortAndVolumeShorthands(t *testing.T) {
	for in, want := range map[string]model.Port{
		`"8080"`:              {Host: 8080, Container: 8080},
		`"8080:80"`:           {Host: 8080, Container: 80},
		`"127.0.0.1:8080:80"`: {Host: 8080, Container: 80, HostIP: "127.0.0.1"},
		`"8080:80/udp"`:       {Host: 8080, Container: 80, Protocol: "udp"},
		`{"host": 1, "container": 2, "protocol": "udp"}`: {Host: 1, Container: 2, Protocol: "udp"},
	} {
		var got model.Port
		if err := json.Unmarshal([]byte(in), &got); err != nil || got != want {
			t.Errorf("port %s = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{`"a:b"`, `"1:2:3:4"`, `""`} {
		if err := json.Unmarshal([]byte(bad), new(model.Port)); err == nil {
			t.Errorf("port %s should be rejected", bad)
		}
	}
	for in, want := range map[string]model.Volume{
		`"/srv/data:/data"`:      {Source: "/srv/data", Destination: "/data"},
		`"/srv/data:/data:ro,Z"`: {Source: "/srv/data", Destination: "/data", Options: "ro,Z"},
		`"named:/data"`:          {Source: "named", Destination: "/data"},
	} {
		var got model.Volume
		if err := json.Unmarshal([]byte(in), &got); err != nil || got != want {
			t.Errorf("volume %s = %+v, %v; want %+v", in, got, err, want)
		}
	}
	if err := json.Unmarshal([]byte(`"/only"`), new(model.Volume)); err == nil {
		t.Error("a volume needs a destination")
	}
	// What the loader reads is what the renderer writes back.
	if (model.Port{Host: 8080, Container: 80, HostIP: "127.0.0.1", Protocol: "udp"}).String() != "127.0.0.1:8080:80/udp" {
		t.Error("port round trip")
	}
	if (model.Volume{Source: "/a", Destination: "/b", Options: "ro"}).String() != "/a:/b:ro" {
		t.Error("volume round trip")
	}
}

// Networks have no Kubernetes equivalent, so they arrive as an annotation and
// are sorted and deduplicated on the way in. The Network= lines they become are
// compared byte for byte, so the order a human wrote them must not matter -
// the manifest still carries the annotation verbatim, as authored.
func TestNetworksComeFromTheAnnotationSortedAndDeduplicated(t *testing.T) {
	spec := func(networks string) map[string]string {
		files := baseFiles()
		files["apps/api.yaml"] = `
apiVersion: v1
kind: Pod
metadata:
  name: api
  annotations:
    io.podcd.networks: "` + networks + `"
spec:
  containers:
    - name: api
      image: example.com/api` + digest + `
`
		return files
	}
	a, err := resolveOne(t, spec("b, a"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := resolveOne(t, spec("a,b,  a"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(a.Networks, ",") != "a,b" {
		t.Errorf("networks should be sorted and deduplicated, got %v", a.Networks)
	}
	if strings.Join(a.Networks, ",") != strings.Join(b.Networks, ",") {
		t.Errorf("the same networks in a different order must compile the same: %v vs %v", a.Networks, b.Networks)
	}
	none, err := resolveOne(t, baseFiles())
	if err != nil {
		t.Fatal(err)
	}
	if len(none.Networks) != 0 {
		t.Errorf("no annotation means no networks, got %v", none.Networks)
	}
}

// Every problem with a workload is reported in one go, so the author fixes
// them in one commit rather than one reconcile at a time.
func TestAuthorMistakesAreReportedTogether(t *testing.T) {
	files := baseFiles()
	files["apps/api.yaml"] = `
apiVersion: v1
kind: Pod
metadata:
  name: api
spec:
  containers:
    - name: dup
      image: example.com/one` + digest + `
    - name: dup
    - image: example.com/three` + digest + `
`
	_, err := resolveOne(t, files)
	if err == nil {
		t.Fatal("a spec with this many mistakes must not compile")
	}
	for _, want := range []string{
		`container "dup" is defined twice`,
		`container "dup" has no image`,
		"a container has no name",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got:\n%v", want, err)
		}
	}
	if got := strings.Count(err.Error(), "\n"); got < 2 {
		t.Errorf("want all problems listed on separate lines, got %d newlines:\n%v", got, err)
	}
}

func TestExcludeMustNameSomethingTheHostWouldRun(t *testing.T) {
	files := baseFiles()
	files["hosts/prod-web-01.yaml"] = `
apiVersion: gitops.podcd.io/v1
kind: Host
metadata:
  name: prod-web-01
spec:
  groups: [web]
  excludeApplications: [nothing]
`
	_, err := loadIndex(t, files).Resolve(context.Background(), ResolveOptions{Host: "prod-web-01"})
	if err == nil || !strings.Contains(err.Error(), `excludeApplications lists "nothing"`) {
		t.Fatalf("a stale exclude is a mistake worth reporting, got: %v", err)
	}
}

func TestUnknownEnvironmentAndGroupNameTheKnownOnes(t *testing.T) {
	files := baseFiles()
	files["hosts/prod-web-01.yaml"] = `
apiVersion: gitops.podcd.io/v1
kind: Host
metadata:
  name: prod-web-01
spec:
  environment: staging
`
	_, err := loadIndex(t, files).Resolve(context.Background(), ResolveOptions{Host: "prod-web-01"})
	if err == nil || !strings.Contains(err.Error(), `environment "staging" is not defined (known: production)`) {
		t.Fatalf("got: %v", err)
	}
	files["hosts/prod-web-01.yaml"] = strings.Replace(files["hosts/prod-web-01.yaml"], "environment: staging", "groups: [wbe]", 1)
	_, err = loadIndex(t, files).Resolve(context.Background(), ResolveOptions{Host: "prod-web-01"})
	if err == nil || !strings.Contains(err.Error(), `group "wbe" is not defined (known: web)`) {
		t.Fatalf("got: %v", err)
	}
}

func TestOverrideWithUnknownFieldIsRejectedWithItsLayer(t *testing.T) {
	files := baseFiles()
	files["envs/production.yaml"] = `
apiVersion: gitops.podcd.io/v1
kind: Environment
metadata:
  name: production
spec:
  overrides:
    api:
      spec:
        containers:
          - name: api
            imagee: nope
`
	_, err := resolveOne(t, files)
	if err == nil || !strings.Contains(err.Error(), "environment/production: override for pod \"api\"") || !strings.Contains(err.Error(), "imagee") {
		t.Fatalf("the layer and the field should be named, got: %v", err)
	}
}

func TestProvenanceListsEveryLayerThatTouchedTheApplication(t *testing.T) {
	app, err := resolveOne(t, baseFiles())
	if err != nil {
		t.Fatal(err)
	}
	if len(app.Origins) != 3 || !strings.HasPrefix(app.Origins[0], "test/apps/api.yaml:") ||
		app.Origins[1] != "environment/production" || app.Origins[2] != "group/web" {
		t.Errorf("origins = %v", app.Origins)
	}
	if app.SourceRepo != "test" {
		t.Errorf("source repo = %q", app.SourceRepo)
	}
}

func TestNonYAMLFilesAndDotDirectoriesAreIgnored(t *testing.T) {
	files := baseFiles()
	files["README.md"] = "apiVersion: nonsense\n"
	files["scripts/deploy.sh"] = "kind: Pod\n"
	files[".github/workflows/ci.yaml"] = "kind: Pod\nmetadata: {name: ci}\n"
	files["apps/notes.txt"] = "kind: Host\n"
	ix := loadIndex(t, files)
	if len(ix.Pods) != 2 || len(ix.Hosts) != 1 {
		t.Fatalf("only .yaml/.yml outside dot-directories count: pods=%d hosts=%d", len(ix.Pods), len(ix.Hosts))
	}
}

func TestForeignKubernetesKindsAreRefusedNotIgnored(t *testing.T) {
	files := baseFiles()
	files["apps/deploy.yaml"] = "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: d}\n"
	if err := NewIndex().LoadTree("test", writeTree(t, files)); err == nil || !strings.Contains(err.Error(), "apps/v1") {
		t.Fatalf("a Deployment cannot be played by podman and must not be silently skipped, got: %v", err)
	}
}
