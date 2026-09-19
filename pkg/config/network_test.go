package config

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/podcd/podcd/pkg/model"
)

// podOn returns a Pod document that joins the given networks.
func podOn(name, networks string) string {
	return `
apiVersion: v1
kind: Pod
metadata:
  name: ` + name + `
  annotations:
    io.podcd.networks: "` + networks + `"
spec:
  containers:
    - name: ` + name + `
      image: example.com/` + name + digest + `
`
}

const hostVM1 = `
apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: vm-1}
spec: {applications: [api, worker]}
`

func resolveVM1(t *testing.T, files map[string]string) (model.DesiredState, error) {
	t.Helper()
	return loadIndex(t, files).Resolve(context.Background(), ResolveOptions{Host: "vm-1"})
}

// A Network document makes the network podcd's to create; a pod naming it
// records that, and a name with no document is left as it is.
func TestNetworksAreDesiredOnlyWhenAPodJoinsThem(t *testing.T) {
	files := map[string]string{
		"apps/api.yaml":    podOn("api", "backend,podman"),
		"apps/worker.yaml": podOn("worker", "backend"),
		"host.yaml":        hostVM1,
		"networks.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Network
metadata: {name: backend}
spec:
  subnet: 10.90.0.0/24
  gateway: 10.90.0.1
  internal: true
  dns: [10.90.0.53]
  options: {mtu: "1400"}
---
apiVersion: gitops.podcd.io/v1
kind: Network
metadata: {name: unused}
spec: {}
`,
	}
	got, err := resolveVM1(t, files)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Networks) != 1 || got.Networks[0].Name != "backend" {
		t.Fatalf("want only backend desired (unused joins nothing), got %+v", got.Networks)
	}
	n := got.Networks[0]
	if n.Subnet != "10.90.0.0/24" || n.Gateway != "10.90.0.1" || !n.Internal || len(n.DNS) != 1 || n.Options["mtu"] != "1400" {
		t.Fatalf("spec did not carry over: %+v", n)
	}
	if !strings.Contains(n.Origins[0], "networks.yaml:") {
		t.Fatalf("want provenance, got %v", n.Origins)
	}
	api, _ := got.App("api")
	if strings.Join(api.Networks, ",") != "backend,podman" {
		t.Fatalf("networks: %v", api.Networks)
	}
	if strings.Join(api.ManagedNetworks, ",") != "backend" {
		t.Fatalf("only backend has a document, got managed %v", api.ManagedNetworks)
	}
}

func TestNetworkNameMustBeUsableAsAUnitAndPodmanName(t *testing.T) {
	files := map[string]string{
		"apps/api.yaml":    podOn("api", "Bad_Name"),
		"apps/worker.yaml": podOn("worker", ""),
		"host.yaml":        hostVM1,
		"net.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Network
metadata: {name: Bad_Name}
spec: {}
`,
	}
	_, err := resolveVM1(t, files)
	if err == nil || !strings.Contains(err.Error(), `network "Bad_Name"`) {
		t.Fatalf("want the name refused, got %v", err)
	}
}

func TestNetworkSpecIsDecodedStrictly(t *testing.T) {
	_, _, err := lintFiles(t, map[string]string{
		"host.yaml": hostVM1,
		"net.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Network
metadata: {name: backend}
spec: {subnett: 10.0.0.0/24}
`,
	})
	if err == nil || !strings.Contains(err.Error(), "subnett") {
		t.Fatalf("want the misspelled field refused, got %v", err)
	}
}

// A Network is infrastructure a host provides, so it may be templated - a
// subnet that differs per host is the obvious case.
func TestNetworksMayBeTemplated(t *testing.T) {
	files := map[string]string{
		"apps/api.yaml":    podOn("api", "backend"),
		"apps/worker.yaml": podOn("worker", ""),
		"host.yaml":        hostVM1,
		"net.yaml.tpl": `
apiVersion: gitops.podcd.io/v1
kind: Network
metadata: {name: backend}
spec:
  subnet: '{{ .Values.subnet }}'
`,
	}
	got, err := loadIndex(t, files).Resolve(context.Background(), ResolveOptions{Host: "vm-1", Values: Values{"subnet": "10.7.0.0/24"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Networks) != 1 || got.Networks[0].Subnet != "10.7.0.0/24" {
		t.Fatalf("want the rendered subnet, got %+v", got.Networks)
	}
}

func TestARenderedNetworkMayNotShadowAPlainOne(t *testing.T) {
	files := map[string]string{
		"apps/api.yaml":    podOn("api", "backend"),
		"apps/worker.yaml": podOn("worker", ""),
		"host.yaml":        hostVM1,
		"net.yaml":         "apiVersion: gitops.podcd.io/v1\nkind: Network\nmetadata: {name: backend}\nspec: {}\n",
		"net.yaml.tpl":     "apiVersion: gitops.podcd.io/v1\nkind: Network\nmetadata: {name: backend}\nspec: {}\n",
	}
	_, err := resolveVM1(t, files)
	if err == nil || !strings.Contains(err.Error(), `Network "backend" is defined twice`) {
		t.Fatalf("want the duplicate refused, got %v", err)
	}
}

// recordingProvisioner is a SecretProvisioner that remembers which
// ExternalSecrets Resolve handed it, and answers from those.
type recordingProvisioner struct {
	added map[string]Doc[ExternalSecretSpec]
}

func (r *recordingProvisioner) AddExternalSecrets(docs map[string]Doc[ExternalSecretSpec]) {
	if r.added == nil {
		r.added = map[string]Doc[ExternalSecretSpec]{}
	}
	for k, v := range docs {
		r.added[k] = v
	}
}

func (r *recordingProvisioner) ProvisionSecret(_ context.Context, name string) (corev1.Secret, bool, error) {
	for esName, es := range r.added {
		target := es.Spec.Target.Name
		if target == "" {
			target = esName
		}
		if target == name {
			return corev1.Secret{Data: map[string][]byte{"key": []byte(es.Spec.Data[0].RemoteRef.Key)}}, true, nil
		}
	}
	return corev1.Secret{}, false, nil
}

// An ExternalSecret decides nothing about which values a host gets, so it
// may be a template; the rendered document reaches the provisioner.
func TestExternalSecretsMayBeTemplatedAndReachTheProvisioner(t *testing.T) {
	files := map[string]string{
		"apps/api.yaml": `
apiVersion: v1
kind: Pod
metadata: {name: api}
spec:
  containers:
    - name: api
      image: example.com/api` + digest + `
      envFrom:
        - secretRef: {name: api-keys}
`,
		"apps/worker.yaml": podOn("worker", ""),
		"host.yaml":        hostVM1,
		"store.yaml": `
apiVersion: external-secrets.io/v1beta1
kind: SecretStore
metadata: {name: local}
spec: {provider: {env: {}}}
`,
		"secret.yaml.tpl": `
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata: {name: api-keys}
spec:
  secretStoreRef: {name: local}
  target: {name: api-keys}
  data:
    - secretKey: TOKEN
      remoteRef: {key: '{{ .Values.tokenVar }}'}
`,
	}
	prov := &recordingProvisioner{}
	got, err := loadIndex(t, files).Resolve(context.Background(), ResolveOptions{Host: "vm-1", Secrets: prov, Values: Values{"tokenVar": "API_TOKEN_VM1"}})
	if err != nil {
		t.Fatal(err)
	}
	es, ok := prov.added["api-keys"]
	if !ok || es.Spec.Data[0].RemoteRef.Key != "API_TOKEN_VM1" {
		t.Fatalf("the rendered ExternalSecret did not reach the provisioner: %+v", prov.added)
	}
	api, _ := got.App("api")
	if !strings.Contains(string(api.Manifest), "kind: Secret") {
		t.Fatal("the provisioned Secret is not in the manifest")
	}

	// Without a provisioner - as podcd lint runs - the rendered ExternalSecret
	// still vouches for the Secret name.
	if _, err := resolveVM1(t, files); err != nil {
		t.Fatalf("lint-style resolve should accept a Secret a rendered ExternalSecret produces: %v", err)
	}
}

func TestSelectorsStillCannotBeTemplated(t *testing.T) {
	files := map[string]string{
		"apps/api.yaml":    podOn("api", ""),
		"apps/worker.yaml": podOn("worker", ""),
		"host.yaml":        hostVM1,
		"group.yaml.tpl":   "apiVersion: gitops.podcd.io/v1\nkind: Group\nmetadata: {name: g}\nspec: {}\n",
	}
	_, err := resolveVM1(t, files)
	if err == nil || !strings.Contains(err.Error(), "not a Group") {
		t.Fatalf("want a templated Group refused, got %v", err)
	}
}

// Overrides for a network layer like those for an application: environment,
// then groups in the host's order, then the host. A named field is set, an
// unnamed one kept, options merged key by key, lists replaced.
func TestNetworkOverridesLayerInPrecedenceOrder(t *testing.T) {
	files := map[string]string{
		"apps/api.yaml":    podOn("api", "backend"),
		"apps/worker.yaml": podOn("worker", ""),
		"net.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Network
metadata: {name: backend}
spec:
  subnet: 10.90.0.0/24
  dns: [10.90.0.53]
  options: {mtu: "1400", isolate: "true"}
`,
		"env.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Environment
metadata: {name: prod}
spec:
  networkOverrides:
    backend:
      internal: true
      options: {mtu: "1500"}
`,
		"group.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Group
metadata: {name: edge}
spec:
  applications: [api, worker]
  networkOverrides:
    backend:
      dns: [10.90.0.54, 10.90.0.55]
      options: {vlan: "7"}
`,
		"host.yaml": `
apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: vm-1}
spec:
  environment: prod
  groups: [edge]
  networkOverrides:
    backend:
      subnet: 10.91.0.0/24
`,
	}
	got, err := resolveVM1(t, files)
	if err != nil {
		t.Fatal(err)
	}
	n := got.Networks[0]
	if n.Subnet != "10.91.0.0/24" || !n.Internal {
		t.Fatalf("host and environment overrides did not apply: %+v", n)
	}
	if strings.Join(n.DNS, ",") != "10.90.0.54,10.90.0.55" {
		t.Fatalf("a list is replaced by the layer that names it, got %v", n.DNS)
	}
	if n.Options["mtu"] != "1500" || n.Options["isolate"] != "true" || n.Options["vlan"] != "7" {
		t.Fatalf("options merge key by key across layers, got %v", n.Options)
	}
	if strings.Join(n.Origins, " ") != "test/net.yaml:2 environment/prod group/edge host/vm-1" {
		t.Fatalf("provenance: %v", n.Origins)
	}
}

func TestNetworkOverrideMistakesAreRefused(t *testing.T) {
	base := func() map[string]string {
		return map[string]string{
			"apps/api.yaml":    podOn("api", "backend"),
			"apps/worker.yaml": podOn("worker", ""),
			"net.yaml":         "apiVersion: gitops.podcd.io/v1\nkind: Network\nmetadata: {name: backend}\nspec: {}\n",
		}
	}
	for name, tc := range map[string]struct{ host, want string }{
		"unknown field": {`
apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: vm-1}
spec:
  applications: [api, worker]
  networkOverrides:
    backend: {subnett: 10.0.0.0/24}
`, "subnett"},
		"host overrides a network it does not use": {`
apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: vm-1}
spec:
  applications: [worker]
  networkOverrides:
    backend: {internal: true}
`, `overrides network "backend", which no application on host "vm-1" joins`},
		"override for a network nobody defines": {`
apiVersion: gitops.podcd.io/v1
kind: Host
metadata: {name: vm-1}
spec:
  applications: [api, worker]
  networkOverrides:
    nowhere: {internal: true}
`, `network "nowhere"`},
	} {
		files := base()
		files["host.yaml"] = tc.host
		_, err := resolveVM1(t, files)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want an error mentioning %q, got %v", name, tc.want, err)
		}
	}
}
