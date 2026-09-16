package config

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	sigyaml "sigs.k8s.io/yaml"

	"github.com/podcd/podcd/pkg/model"
)

// Labels podcd stamps on every pod it plays, so it can recognise its own work.
const (
	LabelManaged = "io.podcd.managed"
	LabelApp     = "io.podcd.app"
)

func patchPod(pod corev1.Pod, override Override) (corev1.Pod, error) {
	if len(bytes.TrimSpace(override)) == 0 {
		return pod, nil
	}
	original, err := json.Marshal(pod)
	if err != nil {
		return pod, err
	}
	patched, err := strategicpatch.StrategicMergePatch(original, []byte(override), corev1.Pod{})
	if err != nil {
		return pod, fmt.Errorf("strategic merge patch: %w", err)
	}
	var out corev1.Pod
	if err := sigyaml.UnmarshalStrict(patched, &out); err != nil {
		return pod, fmt.Errorf("patched pod is not valid: %w", err)
	}
	return out, nil
}

func (h hostDocuments) podToApplication(ctx context.Context, name string, pod corev1.Pod, provisioner SecretProvisioner) (model.Application, error) {
	p := problems{prefix: fmt.Sprintf("pod %q: ", name)}

	if !validName(name) {
		p.add("name must be lowercase letters, digits and dashes")
	}
	if len(pod.Spec.Containers) == 0 {
		p.add("has no containers")
	}

	app := model.Application{
		Name:          name,
		RestartPolicy: kubeRestartPolicy(pod.Spec.RestartPolicy),
		Labels:        maps.Clone(pod.Labels),
	}

	seenNames := map[string]bool{}
	for _, c := range allContainers(pod) {
		if c.Name == "" {
			p.add("a container has no name")
		} else if seenNames[c.Name] {
			p.add("container %q is defined twice", c.Name)
		}
		seenNames[c.Name] = true
		if c.Image == "" {
			p.add("container %q has no image", c.Name)
		}
		app.Images = append(app.Images, c.Image)
		for _, cp := range c.Ports {
			if cp.HostPort == 0 {
				continue
			}
			app.Ports = append(app.Ports, model.Port{
				Host:      int(cp.HostPort),
				Container: int(cp.ContainerPort),
				Protocol:  cmp.Or(strings.ToLower(string(cp.Protocol)), "tcp"),
				HostIP:    cp.HostIP,
			})
		}
	}
	if len(app.Images) > 0 {
		app.Image = app.Images[len(app.Images)-1] // the main container comes last
	}
	slices.SortFunc(app.Ports, comparePorts)

	// Populate Env and Volumes from the first container so callers can inspect the
	// effective config without parsing the manifest YAML.
	if len(pod.Spec.Containers) > 0 {
		c := pod.Spec.Containers[0]
		for _, e := range c.Env {
			if e.ValueFrom == nil {
				if app.Env == nil {
					app.Env = make(map[string]string)
				}
				app.Env[e.Name] = e.Value
			}
		}
		// Map volume mounts back to model.Volume via pod.Spec.Volumes lookup.
		volByName := make(map[string]corev1.Volume, len(pod.Spec.Volumes))
		for _, v := range pod.Spec.Volumes {
			volByName[v.Name] = v
		}
		for _, m := range c.VolumeMounts {
			pv, ok := volByName[m.Name]
			if !ok || pv.HostPath == nil {
				continue
			}
			v := model.Volume{Source: pv.HostPath.Path, Destination: m.MountPath}
			if m.ReadOnly {
				v.Options = "ro"
			}
			app.Volumes = append(app.Volumes, v)
		}
	}

	// ConfigMaps and Secrets the pod refers to must exist in Git, unless the
	// reference is marked optional.
	// Missing ones would only fail inside podman, later, with a worse message.
	refs := collectRefs(pod)
	var configMaps []corev1.ConfigMap
	for _, cmName := range slices.Sorted(maps.Keys(refs.configMaps)) {
		doc, ok := h.configMap(cmName)
		if !ok {
			if !refs.configMaps[cmName] {
				p.add("refers to ConfigMap %q, which is not defined", cmName)
			}
			continue
		}
		configMaps = append(configMaps, doc.Spec)
	}
	// An ExternalSecret wins over a Git Secret of the same name, and is fetched
	// here rather than up front: only the secrets this pod names are worth a
	// round trip to a store.
	var secretDocs []corev1.Secret
	for _, secName := range slices.Sorted(maps.Keys(refs.secrets)) {
		if provisioner != nil {
			sec, ok, err := provisioner.ProvisionSecret(ctx, secName)
			if err != nil {
				p.add("Secret %q: %v", secName, err)
				continue
			}
			if ok {
				secretDocs = append(secretDocs, sec)
				continue
			}
		}
		doc, ok := h.secret(secName)
		if !ok {
			if !refs.secrets[secName] {
				p.add("refers to Secret %q, which is not defined (define it in Git or provision it with an ExternalSecret)", secName)
			}
			continue
		}
		secretDocs = append(secretDocs, doc.Spec)
	}

	if err := p.err(); err != nil {
		return model.Application{}, err
	}

	// Stamp the pod so Inspect can tell it is ours. Labels on the pod are
	// inherited by its containers in podman.
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	pod.Labels[LabelManaged] = "true"
	pod.Labels[LabelApp] = name

	manifest, err := renderManifest(pod, configMaps, secretDocs)
	if err != nil {
		return model.Application{}, fmt.Errorf("pod %q: %w", name, err)
	}
	app.SetManifest(manifest)
	return app, nil
}

// allContainers lists init containers then regular containers.
func allContainers(pod corev1.Pod) []corev1.Container {
	return slices.Concat(pod.Spec.InitContainers, pod.Spec.Containers)
}

// renderManifest writes the documents podman will play, in a fixed order, so
// the same inputs always yield the same bytes.
func renderManifest(pod corev1.Pod, configMaps []corev1.ConfigMap, secretDocs []corev1.Secret) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("# Managed by podcd - do not edit. Generated from Git; secrets resolved on this host.\n")
	docs := make([]any, 0, 1+len(configMaps)+len(secretDocs))
	for i := range configMaps {
		cm := configMaps[i]
		cm.TypeMeta.APIVersion, cm.TypeMeta.Kind = CoreAPIVersion, KindConfigMap
		docs = append(docs, cm)
	}
	for i := range secretDocs {
		s := secretDocs[i]
		s.TypeMeta.APIVersion, s.TypeMeta.Kind = CoreAPIVersion, KindSecret
		docs = append(docs, s)
	}
	pod.TypeMeta.APIVersion, pod.TypeMeta.Kind = CoreAPIVersion, KindPod
	pod.Status = corev1.PodStatus{}
	docs = append(docs, pod)

	for i, d := range docs {
		if i > 0 {
			b.WriteString("---\n")
		}
		out, err := sigyaml.Marshal(d)
		if err != nil {
			return nil, err
		}
		b.Write(out)
	}
	return b.Bytes(), nil
}

// refSet collects referenced names; the bool is "optional". A name that is
// referenced both optionally and not stays required.
type refSet struct {
	configMaps map[string]bool
	secrets    map[string]bool
}

func mark(into map[string]bool, name string, optional *bool) {
	if name == "" {
		return
	}
	opt := optional != nil && *optional
	if seen, ok := into[name]; ok {
		opt = opt && seen
	}
	into[name] = opt
}

// collectRefs walks every place a Pod can name a ConfigMap or a Secret.
func collectRefs(pod corev1.Pod) refSet {
	r := refSet{configMaps: map[string]bool{}, secrets: map[string]bool{}}
	for _, c := range allContainers(pod) {
		for _, e := range c.EnvFrom {
			if e.ConfigMapRef != nil {
				mark(r.configMaps, e.ConfigMapRef.Name, e.ConfigMapRef.Optional)
			}
			if e.SecretRef != nil {
				mark(r.secrets, e.SecretRef.Name, e.SecretRef.Optional)
			}
		}
		for _, e := range c.Env {
			if e.ValueFrom == nil {
				continue
			}
			if e.ValueFrom.ConfigMapKeyRef != nil {
				mark(r.configMaps, e.ValueFrom.ConfigMapKeyRef.Name, e.ValueFrom.ConfigMapKeyRef.Optional)
			}
			if e.ValueFrom.SecretKeyRef != nil {
				mark(r.secrets, e.ValueFrom.SecretKeyRef.Name, e.ValueFrom.SecretKeyRef.Optional)
			}
		}
	}
	for _, v := range pod.Spec.Volumes {
		if v.ConfigMap != nil {
			mark(r.configMaps, v.ConfigMap.Name, v.ConfigMap.Optional)
		}
		if v.Secret != nil {
			mark(r.secrets, v.Secret.SecretName, v.Secret.Optional)
		}
		if v.Projected != nil {
			for _, s := range v.Projected.Sources {
				if s.ConfigMap != nil {
					mark(r.configMaps, s.ConfigMap.Name, s.ConfigMap.Optional)
				}
				if s.Secret != nil {
					mark(r.secrets, s.Secret.Name, s.Secret.Optional)
				}
			}
		}
	}
	for _, ips := range pod.Spec.ImagePullSecrets {
		mark(r.secrets, ips.Name, nil)
	}
	return r
}

func kubeRestartPolicy(p corev1.RestartPolicy) string {
	switch p {
	case corev1.RestartPolicyOnFailure:
		return "on-failure"
	case corev1.RestartPolicyNever:
		return "no"
	default:
		return "always"
	}
}
