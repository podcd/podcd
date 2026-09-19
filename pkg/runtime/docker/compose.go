package docker

// Docker has no `kube play`. The manifest podcd compiles for a pod is turned
// into a Compose project here, with the same shape podman gives a pod: one
// infra container owns the network namespace and the published ports, and
// every workload container joins it, so containers reach each other on
// localhost and the pod has one address on its networks. Init containers
// become services the rest depend on having completed.
//
// ConfigMaps and Secrets have no Docker object to become. Ones a container
// reads as environment are resolved into the compose file; ones it mounts are
// written as files next to it. Both live in the project directory, which is
// readable by the agent user only.

import (
	"cmp"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	sigyaml "sigs.k8s.io/yaml"

	"github.com/podcd/podcd/pkg/config"
)

// labelInfra marks the pause container, the way podman flags its infra
// container, so it does not count as workload.
const labelInfra = "io.podcd.infra"

// infraService is the compose service that stands for the pod.
const infraService = "infra"

// manifest is the played manifest, parsed: one Pod and what it refers to.
type manifest struct {
	pod        corev1.Pod
	configMaps map[string]corev1.ConfigMap
	secrets    map[string]corev1.Secret
}

// parseManifest reads the multi-document YAML the renderer wrote.
func parseManifest(data []byte) (manifest, error) {
	m := manifest{configMaps: map[string]corev1.ConfigMap{}, secrets: map[string]corev1.Secret{}}
	havePod := false
	for _, doc := range splitDocuments(data) {
		var meta metav1.TypeMeta
		if err := sigyaml.Unmarshal(doc, &meta); err != nil {
			return m, fmt.Errorf("parsing manifest: %w", err)
		}
		switch meta.Kind {
		case "":
			continue
		case config.KindPod:
			if havePod {
				return m, fmt.Errorf("manifest holds more than one Pod")
			}
			if err := sigyaml.Unmarshal(doc, &m.pod); err != nil {
				return m, fmt.Errorf("parsing Pod: %w", err)
			}
			havePod = true
		case config.KindConfigMap:
			var cm corev1.ConfigMap
			if err := sigyaml.Unmarshal(doc, &cm); err != nil {
				return m, fmt.Errorf("parsing ConfigMap: %w", err)
			}
			m.configMaps[cm.Name] = cm
		case config.KindSecret:
			var s corev1.Secret
			if err := sigyaml.Unmarshal(doc, &s); err != nil {
				return m, fmt.Errorf("parsing Secret: %w", err)
			}
			m.secrets[s.Name] = s
		default:
			return m, fmt.Errorf("manifest holds a %s, which docker cannot play", meta.Kind)
		}
	}
	if !havePod {
		return m, fmt.Errorf("manifest holds no Pod")
	}
	return m, nil
}

// splitDocuments cuts a YAML stream on its "---" lines.
func splitDocuments(data []byte) [][]byte {
	var docs [][]byte
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			docs = append(docs, []byte(strings.Join(cur, "\n")+"\n"))
		}
		cur = nil
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "---" {
			flush()
			continue
		}
		cur = append(cur, line)
	}
	flush()
	return docs
}

// The subset of the Compose specification podcd writes. Maps marshal with
// sorted keys, so the same manifest always yields the same file.
type composeFile struct {
	Name     string                    `json:"name"`
	Services map[string]*service       `json:"services"`
	Networks map[string]composeNetwork `json:"networks,omitempty"`
	Volumes  map[string]composeVolume  `json:"volumes,omitempty"`
}

type service struct {
	Image           string                `json:"image"`
	ContainerName   string                `json:"container_name"`
	Hostname        string                `json:"hostname,omitempty"`
	Labels          map[string]string     `json:"labels,omitempty"`
	Restart         string                `json:"restart,omitempty"`
	PullPolicy      string                `json:"pull_policy,omitempty"`
	NetworkMode     string                `json:"network_mode,omitempty"`
	Networks        []string              `json:"networks,omitempty"`
	Ports           []port                `json:"ports,omitempty"`
	Entrypoint      []string              `json:"entrypoint,omitempty"`
	Command         []string              `json:"command,omitempty"`
	Environment     map[string]string     `json:"environment,omitempty"`
	WorkingDir      string                `json:"working_dir,omitempty"`
	User            string                `json:"user,omitempty"`
	Privileged      bool                  `json:"privileged,omitempty"`
	ReadOnly        bool                  `json:"read_only,omitempty"`
	CapAdd          []string              `json:"cap_add,omitempty"`
	CapDrop         []string              `json:"cap_drop,omitempty"`
	Volumes         []string              `json:"volumes,omitempty"`
	DependsOn       map[string]dependency `json:"depends_on,omitempty"`
	Healthcheck     *healthcheck          `json:"healthcheck,omitempty"`
	StopGracePeriod string                `json:"stop_grace_period,omitempty"`
	Deploy          *deploy               `json:"deploy,omitempty"`
}

type port struct {
	Target    int    `json:"target"`
	Published string `json:"published"`
	HostIP    string `json:"host_ip,omitempty"`
	Protocol  string `json:"protocol,omitempty"`
}

type dependency struct {
	Condition string `json:"condition"`
}

type healthcheck struct {
	Test        []string `json:"test"`
	Interval    string   `json:"interval,omitempty"`
	Timeout     string   `json:"timeout,omitempty"`
	Retries     int      `json:"retries,omitempty"`
	StartPeriod string   `json:"start_period,omitempty"`
}

type deploy struct {
	Resources struct {
		Limits map[string]string `json:"limits,omitempty"`
	} `json:"resources"`
}

type composeNetwork struct {
	External bool `json:"external,omitempty"`
}

type composeVolume struct {
	Name string `json:"name,omitempty"`
}

// file is something Apply writes beside the compose file: a ConfigMap key or
// a Secret key a container mounts.
type file struct {
	path string
	data []byte
	mode os.FileMode
}

// projectName is the Compose project an application runs as.
func projectName(app string) string { return "podcd-" + app }

// compose translates one application's manifest into a Compose project
// rooted at dir. It is pure: nothing is written, the files to write come
// back with the project.
func compose(app string, m manifest, dir, pauseImage string) (composeFile, []file, error) {
	pod := m.pod
	cf := composeFile{Name: projectName(app), Services: map[string]*service{}}
	labels := maps.Clone(pod.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[config.LabelManaged] = "true"
	labels[config.LabelApp] = app

	infra := &service{
		Image:         pauseImage,
		ContainerName: app + "-" + infraService,
		Labels:        maps.Clone(labels),
		Restart:       "always",
	}
	infra.Labels[labelInfra] = "true"
	if pod.Spec.HostNetwork {
		infra.NetworkMode = "host" // docker refuses a hostname here
	} else {
		infra.Hostname = cmp.Or(pod.Spec.Hostname, pod.Name)
		infra.Ports = publishedPorts(pod)
		for _, n := range podNetworks(pod) {
			infra.Networks = append(infra.Networks, n)
			if cf.Networks == nil {
				cf.Networks = map[string]composeNetwork{}
			}
			cf.Networks[n] = composeNetwork{External: true}
		}
	}
	cf.Services[infraService] = infra

	mounts, files, err := volumeSources(m, dir, &cf)
	if err != nil {
		return cf, nil, err
	}

	restart := restartPolicy(pod.Spec.RestartPolicy)
	var previous []string // init containers, in order; each waits for the one before
	for i, c := range slices.Concat(pod.Spec.InitContainers, pod.Spec.Containers) {
		init := i < len(pod.Spec.InitContainers)
		if c.Name == infraService {
			return cf, nil, fmt.Errorf("container %q: the name is taken by the pod's infra container", c.Name)
		}
		s := &service{
			Image:         c.Image,
			ContainerName: app + "-" + c.Name,
			Labels:        maps.Clone(labels),
			NetworkMode:   "service:" + infraService,
			Entrypoint:    c.Command,
			Command:       c.Args,
			WorkingDir:    c.WorkingDir,
			PullPolicy:    pullPolicy(c.ImagePullPolicy),
			DependsOn:     map[string]dependency{infraService: {Condition: "service_started"}},
			Restart:       restart,
		}
		if init {
			s.Restart = "no"
		}
		for _, p := range previous {
			s.DependsOn[p] = dependency{Condition: "service_completed_successfully"}
		}
		if init {
			previous = append(previous, c.Name)
		}
		if s.Environment, err = environment(c, m); err != nil {
			return cf, nil, fmt.Errorf("container %q: %w", c.Name, err)
		}
		for _, vm := range c.VolumeMounts {
			src, ok := mounts[vm.Name]
			if !ok {
				return cf, nil, fmt.Errorf("container %q mounts volume %q, which the pod does not define", c.Name, vm.Name)
			}
			spec, err := src.mount(vm)
			if err != nil {
				return cf, nil, fmt.Errorf("container %q: %w", c.Name, err)
			}
			s.Volumes = append(s.Volumes, spec)
		}
		if sc := c.SecurityContext; sc != nil {
			if sc.RunAsUser != nil {
				s.User = strconv.FormatInt(*sc.RunAsUser, 10)
				if sc.RunAsGroup != nil {
					s.User += ":" + strconv.FormatInt(*sc.RunAsGroup, 10)
				}
			}
			s.Privileged = sc.Privileged != nil && *sc.Privileged
			s.ReadOnly = sc.ReadOnlyRootFilesystem != nil && *sc.ReadOnlyRootFilesystem
			if caps := sc.Capabilities; caps != nil {
				for _, c := range caps.Add {
					s.CapAdd = append(s.CapAdd, string(c))
				}
				for _, c := range caps.Drop {
					s.CapDrop = append(s.CapDrop, string(c))
				}
			}
		}
		if s.Healthcheck, err = probe(c); err != nil {
			return cf, nil, fmt.Errorf("container %q: %w", c.Name, err)
		}
		if lim := c.Resources.Limits; len(lim) > 0 {
			s.Deploy = &deploy{}
			s.Deploy.Resources.Limits = map[string]string{}
			if q, ok := lim[corev1.ResourceMemory]; ok {
				s.Deploy.Resources.Limits["memory"] = strconv.FormatInt(q.Value(), 10)
			}
			if q, ok := lim[corev1.ResourceCPU]; ok {
				s.Deploy.Resources.Limits["cpus"] = strconv.FormatFloat(float64(q.MilliValue())/1000, 'f', -1, 64)
			}
		}
		if t := pod.Spec.TerminationGracePeriodSeconds; t != nil {
			s.StopGracePeriod = strconv.FormatInt(*t, 10) + "s"
		}
		cf.Services[c.Name] = s
	}
	return cf, files, nil
}

// publishedPorts lists every hostPort in the pod, sorted, for the infra
// container to publish on behalf of the pod.
func publishedPorts(pod corev1.Pod) []port {
	var ports []port
	for _, c := range slices.Concat(pod.Spec.InitContainers, pod.Spec.Containers) {
		for _, cp := range c.Ports {
			if cp.HostPort == 0 {
				continue
			}
			ports = append(ports, port{
				Target: int(cp.ContainerPort), Published: strconv.Itoa(int(cp.HostPort)), HostIP: cp.HostIP,
				Protocol: cmp.Or(strings.ToLower(string(cp.Protocol)), "tcp"),
			})
		}
	}
	slices.SortFunc(ports, func(a, b port) int {
		return cmp.Or(cmp.Compare(a.Published, b.Published), cmp.Compare(a.Target, b.Target), cmp.Compare(a.Protocol, b.Protocol), cmp.Compare(a.HostIP, b.HostIP))
	})
	return ports
}

// podNetworks reads the networks annotation the compiler put on the pod,
// sorted and deduplicated.
func podNetworks(pod corev1.Pod) []string {
	seen := map[string]bool{}
	for _, n := range strings.Split(pod.Annotations[config.AnnotationNetworks], ",") {
		if n = strings.TrimSpace(n); n != "" {
			seen[n] = true
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// restartPolicy maps the pod's policy to Compose's words.
func restartPolicy(p corev1.RestartPolicy) string {
	switch p {
	case corev1.RestartPolicyOnFailure:
		return "on-failure"
	case corev1.RestartPolicyNever:
		return "no"
	default:
		return "always"
	}
}

// pullPolicy maps Kubernetes' words to Compose's.
func pullPolicy(p corev1.PullPolicy) string {
	switch p {
	case corev1.PullAlways:
		return "always"
	case corev1.PullNever:
		return "never"
	default:
		return "missing"
	}
}

// source is where a pod volume comes from on the host: a path to bind, or a
// named volume.
type source struct {
	path   string // bind mount, when set
	volume string // named volume otherwise
}

// mount renders one volumeMount in Compose's short syntax.
func (s source) mount(vm corev1.VolumeMount) (string, error) {
	src := s.volume
	if s.path != "" {
		src = s.path
		if vm.SubPath != "" {
			src = filepath.Join(src, vm.SubPath)
		}
	} else if vm.SubPath != "" {
		return "", fmt.Errorf("volume %q: subPath on a named volume is not supported by docker", vm.Name)
	}
	spec := src + ":" + vm.MountPath
	if vm.ReadOnly {
		spec += ":ro"
	}
	return spec, nil
}

// volumeSources resolves every pod volume. ConfigMaps and Secrets become
// directories of files under dir; emptyDir and persistentVolumeClaim become
// named volumes, which `down` leaves alone.
func volumeSources(m manifest, dir string, cf *composeFile) (map[string]source, []file, error) {
	sources := map[string]source{}
	var files []file
	for _, v := range m.pod.Spec.Volumes {
		switch {
		case v.HostPath != nil:
			sources[v.Name] = source{path: v.HostPath.Path}
		case v.EmptyDir != nil:
			if cf.Volumes == nil {
				cf.Volumes = map[string]composeVolume{}
			}
			cf.Volumes[v.Name] = composeVolume{}
			sources[v.Name] = source{volume: v.Name}
		case v.PersistentVolumeClaim != nil:
			if cf.Volumes == nil {
				cf.Volumes = map[string]composeVolume{}
			}
			cf.Volumes[v.Name] = composeVolume{Name: v.PersistentVolumeClaim.ClaimName}
			sources[v.Name] = source{volume: v.Name}
		case v.ConfigMap != nil:
			cm, ok := m.configMaps[v.ConfigMap.Name]
			if !ok {
				if v.ConfigMap.Optional != nil && *v.ConfigMap.Optional {
					continue
				}
				return nil, nil, fmt.Errorf("volume %q: ConfigMap %q is not in the manifest", v.Name, v.ConfigMap.Name)
			}
			data := map[string][]byte{}
			for k, s := range cm.Data {
				data[k] = []byte(s)
			}
			maps.Copy(data, cm.BinaryData)
			base := filepath.Join(dir, "configmaps", v.Name)
			files = append(files, keyFiles(base, data, v.ConfigMap.Items, 0o644)...)
			sources[v.Name] = source{path: base}
		case v.Secret != nil:
			sec, ok := m.secrets[v.Secret.SecretName]
			if !ok {
				if v.Secret.Optional != nil && *v.Secret.Optional {
					continue
				}
				return nil, nil, fmt.Errorf("volume %q: Secret %q is not in the manifest", v.Name, v.Secret.SecretName)
			}
			base := filepath.Join(dir, "secrets", v.Name)
			files = append(files, keyFiles(base, secretData(sec), v.Secret.Items, 0o600)...)
			sources[v.Name] = source{path: base}
		default:
			return nil, nil, fmt.Errorf("volume %q: only hostPath, emptyDir, persistentVolumeClaim, configMap and secret volumes are supported by docker", v.Name)
		}
	}
	return sources, files, nil
}

// keyFiles lays a ConfigMap or Secret out as one file per key, or as the
// items list says.
func keyFiles(base string, data map[string][]byte, items []corev1.KeyToPath, mode os.FileMode) []file {
	var files []file
	if len(items) == 0 {
		for _, k := range slices.Sorted(maps.Keys(data)) {
			files = append(files, file{path: filepath.Join(base, k), data: data[k], mode: mode})
		}
		return files
	}
	for _, it := range items {
		d, ok := data[it.Key]
		if !ok {
			continue
		}
		files = append(files, file{path: filepath.Join(base, it.Path), data: d, mode: mode})
	}
	return files
}

// secretData merges a Secret's data and stringData, the latter winning.
func secretData(s corev1.Secret) map[string][]byte {
	out := maps.Clone(s.Data)
	if out == nil {
		out = map[string][]byte{}
	}
	for k, v := range s.StringData {
		out[k] = []byte(v)
	}
	return out
}

// environment resolves a container's env and envFrom against the manifest.
// Explicit env entries win over envFrom, as in Kubernetes.
func environment(c corev1.Container, m manifest) (map[string]string, error) {
	env := map[string]string{}
	for _, from := range c.EnvFrom {
		if ref := from.ConfigMapRef; ref != nil {
			cm, ok := m.configMaps[ref.Name]
			if !ok && !(ref.Optional != nil && *ref.Optional) {
				return nil, fmt.Errorf("envFrom ConfigMap %q is not in the manifest", ref.Name)
			}
			for k, v := range cm.Data {
				env[from.Prefix+k] = v
			}
		}
		if ref := from.SecretRef; ref != nil {
			sec, ok := m.secrets[ref.Name]
			if !ok && !(ref.Optional != nil && *ref.Optional) {
				return nil, fmt.Errorf("envFrom Secret %q is not in the manifest", ref.Name)
			}
			for k, v := range secretData(sec) {
				env[from.Prefix+k] = string(v)
			}
		}
	}
	for _, e := range c.Env {
		switch {
		case e.ValueFrom == nil:
			env[e.Name] = e.Value
		case e.ValueFrom.ConfigMapKeyRef != nil:
			ref := e.ValueFrom.ConfigMapKeyRef
			v, ok := m.configMaps[ref.Name].Data[ref.Key]
			if !ok {
				if ref.Optional != nil && *ref.Optional {
					continue
				}
				return nil, fmt.Errorf("env %s: ConfigMap %q has no key %q", e.Name, ref.Name, ref.Key)
			}
			env[e.Name] = v
		case e.ValueFrom.SecretKeyRef != nil:
			ref := e.ValueFrom.SecretKeyRef
			v, ok := secretData(m.secrets[ref.Name])[ref.Key]
			if !ok {
				if ref.Optional != nil && *ref.Optional {
					continue
				}
				return nil, fmt.Errorf("env %s: Secret %q has no key %q", e.Name, ref.Name, ref.Key)
			}
			env[e.Name] = string(v)
		default:
			return nil, fmt.Errorf("env %s: only value, configMapKeyRef and secretKeyRef are supported by docker", e.Name)
		}
	}
	if len(env) == 0 {
		return nil, nil
	}
	return env, nil
}

// probe turns the liveness probe into a Compose healthcheck, the way podman
// does for kube play: exec runs as is, httpGet needs curl in the image and
// tcpSocket needs nc.
func probe(c corev1.Container) (*healthcheck, error) {
	p := c.LivenessProbe
	if p == nil {
		return nil, nil
	}
	h := &healthcheck{}
	switch {
	case p.Exec != nil:
		h.Test = append([]string{"CMD"}, p.Exec.Command...)
	case p.HTTPGet != nil:
		port, err := probePort(c, p.HTTPGet.Port)
		if err != nil {
			return nil, err
		}
		scheme := strings.ToLower(cmp.Or(string(p.HTTPGet.Scheme), "http"))
		url := fmt.Sprintf("%s://%s:%d%s", scheme, cmp.Or(p.HTTPGet.Host, "localhost"), port, p.HTTPGet.Path)
		h.Test = []string{"CMD-SHELL", "curl -f -s -o /dev/null " + url + " || exit 1"}
	case p.TCPSocket != nil:
		port, err := probePort(c, p.TCPSocket.Port)
		if err != nil {
			return nil, err
		}
		h.Test = []string{"CMD-SHELL", fmt.Sprintf("nc -z %s %d || exit 1", cmp.Or(p.TCPSocket.Host, "localhost"), port)}
	default:
		return nil, fmt.Errorf("livenessProbe: only exec, httpGet and tcpSocket are supported by docker")
	}
	if p.PeriodSeconds > 0 {
		h.Interval = strconv.Itoa(int(p.PeriodSeconds)) + "s"
	}
	if p.TimeoutSeconds > 0 {
		h.Timeout = strconv.Itoa(int(p.TimeoutSeconds)) + "s"
	}
	if p.FailureThreshold > 0 {
		h.Retries = int(p.FailureThreshold)
	}
	if p.InitialDelaySeconds > 0 {
		h.StartPeriod = strconv.Itoa(int(p.InitialDelaySeconds)) + "s"
	}
	return h, nil
}

// probePort resolves a probe's port, which may name one of the container's.
func probePort(c corev1.Container, p intstr.IntOrString) (int, error) {
	if p.Type == intstr.Int {
		return p.IntValue(), nil
	}
	for _, cp := range c.Ports {
		if cp.Name == p.StrVal {
			return int(cp.ContainerPort), nil
		}
	}
	return 0, fmt.Errorf("livenessProbe: port %q is not one of the container's named ports", p.StrVal)
}

// marshalCompose renders the project as YAML.
func marshalCompose(cf composeFile) ([]byte, error) {
	out, err := sigyaml.Marshal(cf)
	if err != nil {
		return nil, err
	}
	return append([]byte("# Managed by podcd - do not edit. Generated from the played manifest; secrets resolved on this host.\n"), out...), nil
}
