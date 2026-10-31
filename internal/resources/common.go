// Package resources builds the Kubernetes objects of a cluster from its spec. Every function is pure, so the
// controllers only decide when to apply them.
package resources

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	v1 "github.com/SongOf/pulsar-operator/api/v1alpha1"
)

// Component names, used in labels and object names.
const (
	ZooKeeper    = "zookeeper"
	Bookie       = "bookie"
	AutoRecovery = "autorecovery"
	Broker       = "broker"
	Proxy        = "proxy"
)

// Annotations the operator writes on pod templates.
const (
	AnnotationConfigHash  = "pulsar.songof.dev/config-hash"
	AnnotationRestartedAt = "pulsar.songof.dev/restartedAt"
)

// Labels selects the pods of one component of one cluster.
func Labels(cluster, component string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "pulsar",
		"app.kubernetes.io/instance":   cluster,
		"app.kubernetes.io/component":  component,
		"app.kubernetes.io/managed-by": "pulsar-operator",
	}
}

// Name is the object name of a component of a cluster, such as demo-bookie.
func Name(cluster, component string) string { return cluster + "-" + component }

// PodFQDN is the stable DNS name of a StatefulSet pod behind its headless Service.
func PodFQDN(pod, service, namespace, domain string) string {
	return fmt.Sprintf("%s.%s.%s.svc.%s", pod, service, namespace, domainOr(domain))
}

func domainOr(domain string) string {
	if domain == "" {
		return "cluster.local"
	}
	return domain
}

// ImageOf returns the component image, or the cluster image, or the default.
func ImageOf(component, cluster string) string {
	switch {
	case component != "":
		return component
	case cluster != "":
		return cluster
	default:
		return v1.DefaultImage
	}
}

// JVMOptions renders PULSAR_MEM from a JVM spec, with the component's defaults for unset sizes.
func JVMOptions(j v1.JVMSpec, defaultHeap, defaultDirect string) string {
	heap, direct := j.Heap, j.DirectMemory
	if heap == "" {
		heap = defaultHeap
	}
	if direct == "" {
		direct = defaultDirect
	}
	parts := []string{"-Xms" + heap, "-Xmx" + heap}
	if direct != "" {
		parts = append(parts, "-XX:MaxDirectMemorySize="+direct)
	}
	if j.ExtraOptions != "" {
		parts = append(parts, j.ExtraOptions)
	}
	return strings.Join(parts, " ")
}

// ConfigData merges the operator's defaults with the user's config into the environment the Pulsar image's
// apply-config-from-env.py reads: every key becomes PULSAR_PREFIX_<key>, which adds or replaces it in the
// configuration file. User entries win over defaults, except for keys the operator must own.
func ConfigData(defaults, user map[string]string, owned []string, env map[string]string) map[string]string {
	data := map[string]string{}
	for k, v := range defaults {
		data["PULSAR_PREFIX_"+k] = v
	}
	for k, v := range user {
		data["PULSAR_PREFIX_"+k] = v
	}
	for _, k := range owned {
		if v, ok := defaults[k]; ok {
			data["PULSAR_PREFIX_"+k] = v
		}
	}
	for k, v := range env {
		data[k] = v
	}
	return data
}

// Hash is a short stable digest of a string map, used to restart pods when their configuration changes.
func Hash(data map[string]string) string {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(data[k]))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// ConfigMap holds a component's environment.
func ConfigMap(name, namespace string, labels, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Data:       data,
	}
}

// metricsPorts are where each component serves Prometheus metrics, advertised with the conventional
// prometheus.io annotations so any Prometheus discovers them.
var metricsPorts = map[string][2]string{
	Broker: {"8080", "/metrics/"}, Proxy: {"8080", "/metrics/"}, Bookie: {"8000", "/metrics"}, ZooKeeper: {"8000", "/metrics"},
}

// PodAnnotations are the template annotations: scrape hints, the user's, the config hash and the restart marker.
func PodAnnotations(p v1.PodPolicy, configHash, restartedAt string, component ...string) map[string]string {
	a := map[string]string{}
	if len(component) == 1 {
		if mp, ok := metricsPorts[component[0]]; ok {
			a["prometheus.io/scrape"], a["prometheus.io/port"], a["prometheus.io/path"] = "true", mp[0], mp[1]
		}
	}
	for k, v := range p.Annotations {
		a[k] = v
	}
	a[AnnotationConfigHash] = configHash
	if restartedAt != "" {
		a[AnnotationRestartedAt] = restartedAt
	}
	return a
}

// Affinity spreads the pods matching labels across nodes as the mode asks.
func Affinity(mode v1.AntiAffinityMode, labels map[string]string) *corev1.Affinity {
	term := corev1.PodAffinityTerm{
		LabelSelector: &metav1.LabelSelector{MatchLabels: labels},
		TopologyKey:   "kubernetes.io/hostname",
	}
	switch mode {
	case v1.AntiAffinityNone:
		return nil
	case v1.AntiAffinityRequired:
		return &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{term},
		}}
	default:
		return &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{Weight: 100, PodAffinityTerm: term}},
		}}
	}
}

// PodSpec is the scheduling part every component shares. The Pulsar image runs as uid 10000 in group 0, so
// volumes are made group-writable for group 0.
func PodSpec(p v1.PodPolicy, labels map[string]string, grace int64) corev1.PodSpec {
	return corev1.PodSpec{
		NodeSelector:                  p.NodeSelector,
		Tolerations:                   p.Tolerations,
		Affinity:                      Affinity(p.AntiAffinity, labels),
		PriorityClassName:             p.PriorityClassName,
		TerminationGracePeriodSeconds: ptr.To(grace),
		SecurityContext: &corev1.PodSecurityContext{
			FSGroup:             ptr.To[int64](0),
			FSGroupChangePolicy: ptr.To(corev1.FSGroupChangeOnRootMismatch),
		},
	}
}

// PodEnv exposes the pod's own name and namespace, from which the containers build their FQDN.
func PodEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
		{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
	}
}

// EnvFrom loads a component's ConfigMap into the container environment.
func EnvFrom(configMap string) []corev1.EnvFromSource {
	return []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: configMap}}}}
}

// Shell runs a script with bash, as the Pulsar image's helper scripts need.
func Shell(script string) []string { return []string{"bash", "-c", script} }

// HeadlessService gives every StatefulSet pod a stable DNS name, also before it is ready, so ZooKeeper quorum
// members and bookies can resolve each other while starting.
func HeadlessService(name, namespace string, labels map[string]string, ports []corev1.ServicePort) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: corev1.ServiceSpec{
			ClusterIP:                corev1.ClusterIPNone,
			Selector:                 labels,
			Ports:                    ports,
			PublishNotReadyAddresses: true,
		},
	}
}

// Port is a TCP service port.
func Port(name string, port int32) corev1.ServicePort {
	return corev1.ServicePort{Name: name, Port: port, Protocol: corev1.ProtocolTCP}
}

// ContainerPorts converts service ports into container ports.
func ContainerPorts(ports []corev1.ServicePort) []corev1.ContainerPort {
	out := make([]corev1.ContainerPort, 0, len(ports))
	for _, p := range ports {
		out = append(out, corev1.ContainerPort{Name: p.Name, ContainerPort: p.Port, Protocol: corev1.ProtocolTCP})
	}
	return out
}
