package resources

import (
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	v1 "github.com/SongOf/pulsar-operator/api/v1alpha1"
)

// Pulsar builds the ZooKeeper, broker, proxy and metadata objects of a PulsarCluster. Its bookies come from a
// child BookKeeperCluster, built by ChildBookKeeper.
type Pulsar struct{ C *v1.PulsarCluster }

func (p Pulsar) name(component string) string { return Name(p.C.Name, component) }
func (p Pulsar) ns() string                    { return p.C.Namespace }
func (p Pulsar) domain() string                { return domainOr(p.C.Spec.ClusterDomain) }

// ZooKeeperName is the ZooKeeper StatefulSet and headless Service.
func (p Pulsar) ZooKeeperName() string { return p.name(ZooKeeper) }

// ZooKeeperReplicas defaults to three.
func (p Pulsar) ZooKeeperReplicas() int32 {
	if p.C.Spec.ZooKeeper.Replicas == 0 {
		return 3
	}
	return p.C.Spec.ZooKeeper.Replicas
}

// ZooKeeperConnect lists every ZooKeeper server, as host:port,host:port.
func (p Pulsar) ZooKeeperConnect() string {
	hosts := make([]string, 0, p.ZooKeeperReplicas())
	for i := int32(0); i < p.ZooKeeperReplicas(); i++ {
		pod := fmt.Sprintf("%s-%d", p.ZooKeeperName(), i)
		hosts = append(hosts, PodFQDN(pod, p.ZooKeeperName(), p.ns(), p.domain())+":2181")
	}
	return strings.Join(hosts, ",")
}

// MetadataStoreURL is the Pulsar metadata store URL.
func (p Pulsar) MetadataStoreURL() string { return "zk:" + p.ZooKeeperConnect() }

// BrokerName is the broker StatefulSet; BrokerServiceName is the client Service.
func (p Pulsar) BrokerName() string        { return p.name(Broker) }
func (p Pulsar) BrokerHeadlessName() string { return p.name(Broker) + "-headless" }
func (p Pulsar) BrokerServiceName() string { return p.name(Broker) }
func (p Pulsar) ProxyName() string         { return p.name(Proxy) }

// ServiceURL and WebServiceURL are the in-cluster client addresses of the brokers.
func (p Pulsar) ServiceURL() string {
	return fmt.Sprintf("pulsar://%s.%s.svc.%s:6650", p.BrokerServiceName(), p.ns(), p.domain())
}
func (p Pulsar) WebServiceURL() string {
	return fmt.Sprintf("http://%s.%s.svc.%s:8080", p.BrokerServiceName(), p.ns(), p.domain())
}

// ChildBookKeeper is the BookKeeperCluster that holds this cluster's bookies.
func (p Pulsar) ChildBookKeeper() *v1.BookKeeperCluster {
	spec := p.C.Spec.BookKeeper
	spec.Image = ImageOf(spec.Image, p.C.Spec.Image)
	return &v1.BookKeeperCluster{
		ObjectMeta: metav1.ObjectMeta{Name: p.C.Name, Namespace: p.ns(), Labels: Labels(p.C.Name, Bookie)},
		Spec: v1.BookKeeperClusterSpec{
			ZooKeeper:       p.ZooKeeperConnect(),
			LedgersRootPath: "/ledgers",
			ClusterDomain:   p.domain(),
			BookKeeperSpec:  spec,
		},
	}
}

// ---- ZooKeeper ----

func (p Pulsar) zkPorts() []corev1.ServicePort {
	return []corev1.ServicePort{Port("client", 2181), Port("follower", 2888), Port("election", 3888)}
}

// ZooKeeperConfigMap is the ZooKeeper configuration.
func (p Pulsar) ZooKeeperConfigMap() *corev1.ConfigMap {
	z := p.C.Spec.ZooKeeper
	servers := make([]string, 0, p.ZooKeeperReplicas())
	for i := int32(0); i < p.ZooKeeperReplicas(); i++ {
		servers = append(servers, fmt.Sprintf("%s-%d", p.ZooKeeperName(), i))
	}
	defaults := map[string]string{
		"dataDir":                "/pulsar/data/zookeeper",
		"quorumListenOnAllIPs":   "true",
		"4lw.commands.whitelist": "*",
	}
	env := map[string]string{
		"PULSAR_MEM":        JVMOptions(z.JVM, "512m", ""),
		"ZOOKEEPER_SERVERS": strings.Join(servers, ","),
	}
	return ConfigMap(p.ZooKeeperName()+"-config", p.ns(), Labels(p.C.Name, ZooKeeper), ConfigData(defaults, z.Config, []string{"dataDir"}, env))
}

// ZooKeeperService is the headless Service of the ensemble.
func (p Pulsar) ZooKeeperService() *corev1.Service {
	return HeadlessService(p.ZooKeeperName(), p.ns(), Labels(p.C.Name, ZooKeeper), p.zkPorts())
}

// ZooKeeperStatefulSet runs the ensemble; generate-zookeeper-config.sh derives each server's id from its hostname.
func (p Pulsar) ZooKeeperStatefulSet(configHash string) *appsv1.StatefulSet {
	z := p.C.Spec.ZooKeeper
	labels := Labels(p.C.Name, ZooKeeper)
	pod := PodSpec(z.Pod, labels, 30)
	pod.Containers = []corev1.Container{{
		Name:  ZooKeeper,
		Image: ImageOf(z.Image, p.C.Spec.Image),
		Command: Shell("bin/apply-config-from-env.py conf/zookeeper.conf && " +
			"bin/generate-zookeeper-config.sh conf/zookeeper.conf && exec bin/pulsar zookeeper"),
		Env:            PodEnv(),
		EnvFrom:        EnvFrom(p.ZooKeeperName() + "-config"),
		Ports:          ContainerPorts(p.zkPorts()),
		Resources:      z.Resources,
		ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"bin/pulsar-zookeeper-ruok.sh"}}}, InitialDelaySeconds: 10, PeriodSeconds: 10, TimeoutSeconds: 5, FailureThreshold: 6},
		LivenessProbe:  tcpProbe(2181, 60, 30, 5),
		VolumeMounts:   []corev1.VolumeMount{{Name: "data", MountPath: "/pulsar/data/zookeeper"}},
	}}
	claim := corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data", Labels: labels},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: z.Storage.StorageClassName,
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: z.Storage.Size}},
		},
	}
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: p.ZooKeeperName(), Namespace: p.ns(), Labels: labels},
		Spec: appsv1.StatefulSetSpec{
			Replicas:            ptr.To(p.ZooKeeperReplicas()),
			ServiceName:         p.ZooKeeperName(),
			Selector:            &metav1.LabelSelector{MatchLabels: labels},
			PodManagementPolicy: appsv1.ParallelPodManagement,
			UpdateStrategy:      appsv1.StatefulSetUpdateStrategy{Type: appsv1.RollingUpdateStatefulSetStrategyType},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: PodAnnotations(z.Pod, configHash, z.RestartedAt, ZooKeeper)},
				Spec:       pod,
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{claim},
		},
	}
}

// ---- metadata ----

// MetadataJobName initialises the cluster metadata.
func (p Pulsar) MetadataJobName() string { return p.name("metadata-init") }

// MetadataJob registers the cluster in the metadata store. It formats BookKeeper metadata only when absent, so it
// is safe after the BookKeeper init job.
func (p Pulsar) MetadataJob() *batchv1.Job {
	labels := Labels(p.C.Name, "metadata-init")
	pod := PodSpec(v1.PodPolicy{NodeSelector: p.C.Spec.Broker.Pod.NodeSelector, Tolerations: p.C.Spec.Broker.Pod.Tolerations, AntiAffinity: v1.AntiAffinityNone}, labels, 30)
	pod.RestartPolicy = corev1.RestartPolicyOnFailure
	script := fmt.Sprintf("exec bin/pulsar initialize-cluster-metadata --cluster %s --metadata-store %s "+
		"--configuration-metadata-store %s --web-service-url %s --broker-service-url %s",
		p.C.Name, p.MetadataStoreURL(), p.MetadataStoreURL(), p.WebServiceURL(), p.ServiceURL())
	pod.Containers = []corev1.Container{{
		Name: "metadata-init", Image: ImageOf(p.C.Spec.Broker.Image, p.C.Spec.Image), Command: Shell(script),
		Env: []corev1.EnvVar{{Name: "PULSAR_MEM", Value: "-Xms256m -Xmx256m"}},
	}}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: p.MetadataJobName(), Namespace: p.ns(), Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To[int32](6),
			Template:     corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: pod},
		},
	}
}

// ---- broker ----

func (p Pulsar) brokerPorts() []corev1.ServicePort {
	return []corev1.ServicePort{Port("pulsar", 6650), Port("http", 8080)}
}

// BrokerConfigMap is the broker configuration.
func (p Pulsar) BrokerConfigMap() *corev1.ConfigMap {
	b := p.C.Spec.Broker
	defaults := map[string]string{
		"clusterName":                    p.C.Name,
		"metadataStoreUrl":               p.MetadataStoreURL(),
		"configurationMetadataStoreUrl":  p.MetadataStoreURL(),
		"brokerServicePort":              "6650",
		"webServicePort":                 "8080",
		"functionsWorkerEnabled":         "false",
		"managedLedgerDefaultEnsembleSize": "2",
		"managedLedgerDefaultWriteQuorum":  "2",
		"managedLedgerDefaultAckQuorum":    "2",
	}
	owned := []string{"clusterName", "metadataStoreUrl", "configurationMetadataStoreUrl", "brokerServicePort", "webServicePort"}
	env := map[string]string{"PULSAR_MEM": JVMOptions(b.JVM, "1g", "1g")}
	return ConfigMap(p.BrokerName()+"-config", p.ns(), Labels(p.C.Name, Broker), ConfigData(defaults, b.Config, owned, env))
}

// BrokerHeadlessService gives brokers the FQDNs they advertise.
func (p Pulsar) BrokerHeadlessService() *corev1.Service {
	return HeadlessService(p.BrokerHeadlessName(), p.ns(), Labels(p.C.Name, Broker), p.brokerPorts())
}

// BrokerService is the address clients and the proxy connect to.
func (p Pulsar) BrokerService() *corev1.Service {
	labels := Labels(p.C.Name, Broker)
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: p.BrokerServiceName(), Namespace: p.ns(), Labels: labels},
		Spec:       corev1.ServiceSpec{Selector: labels, Ports: p.brokerPorts()},
	}
}

// BrokerStatefulSet runs the brokers; each advertises its own FQDN so topic lookups redirect to it.
func (p Pulsar) BrokerStatefulSet(configHash string) *appsv1.StatefulSet {
	b := p.C.Spec.Broker
	labels := Labels(p.C.Name, Broker)
	pod := PodSpec(b.Pod, labels, 60)
	fqdn := fmt.Sprintf("$(POD_NAME).%s.$(POD_NAMESPACE).svc.%s", p.BrokerHeadlessName(), p.domain())
	pod.Containers = []corev1.Container{{
		Name:           Broker,
		Image:          ImageOf(b.Image, p.C.Spec.Image),
		Command:        Shell("bin/apply-config-from-env.py conf/broker.conf && exec bin/pulsar broker"),
		Env:            append(PodEnv(), corev1.EnvVar{Name: "PULSAR_PREFIX_advertisedAddress", Value: fqdn}),
		EnvFrom:        EnvFrom(p.BrokerName() + "-config"),
		Ports:          ContainerPorts(p.brokerPorts()),
		Resources:      b.Resources,
		// /status.html answers 404 unless statusFilePath is configured; /admin/v2/brokers/ready reports
		// whether the broker finished starting and can serve topics.
		ReadinessProbe: httpProbe("/admin/v2/brokers/ready", 8080, 20, 10, 6),
		LivenessProbe:  tcpProbe(8080, 90, 30, 5),
	}}
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: p.BrokerName(), Namespace: p.ns(), Labels: labels},
		Spec: appsv1.StatefulSetSpec{
			Replicas:            ptr.To(b.Replicas),
			ServiceName:         p.BrokerHeadlessName(),
			Selector:            &metav1.LabelSelector{MatchLabels: labels},
			PodManagementPolicy: appsv1.ParallelPodManagement,
			UpdateStrategy:      appsv1.StatefulSetUpdateStrategy{Type: appsv1.RollingUpdateStatefulSetStrategyType},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: PodAnnotations(b.Pod, configHash, b.RestartedAt, Broker)},
				Spec:       pod,
			},
		},
	}
}

// ---- proxy ----

// ProxyConfigMap is the proxy configuration; the proxy forwards to the broker Service.
func (p Pulsar) ProxyConfigMap() *corev1.ConfigMap {
	x := p.C.Spec.Proxy
	defaults := map[string]string{
		"clusterName":          p.C.Name,
		"brokerServiceURL":     p.ServiceURL(),
		"brokerWebServiceURL":  p.WebServiceURL(),
		"servicePort":          "6650",
		"webServicePort":       "8080",
	}
	owned := []string{"clusterName", "brokerServiceURL", "brokerWebServiceURL", "servicePort", "webServicePort"}
	env := map[string]string{"PULSAR_MEM": JVMOptions(x.JVM, "512m", "512m")}
	return ConfigMap(p.ProxyName()+"-config", p.ns(), Labels(p.C.Name, Proxy), ConfigData(defaults, x.Config, owned, env))
}

// ProxyService exposes the proxy with the configured Service type.
func (p Pulsar) ProxyService() *corev1.Service {
	labels := Labels(p.C.Name, Proxy)
	t := p.C.Spec.Proxy.ServiceType
	if t == "" {
		t = corev1.ServiceTypeClusterIP
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: p.ProxyName(), Namespace: p.ns(), Labels: labels},
		Spec:       corev1.ServiceSpec{Type: t, Selector: labels, Ports: p.brokerPorts()},
	}
}

// ProxyDeployment runs the stateless proxies.
func (p Pulsar) ProxyDeployment(configHash string) *appsv1.Deployment {
	x := p.C.Spec.Proxy
	labels := Labels(p.C.Name, Proxy)
	pod := PodSpec(x.Pod, labels, 30)
	pod.Containers = []corev1.Container{{
		Name:           Proxy,
		Image:          ImageOf(x.Image, p.C.Spec.Image),
		Command:        Shell("bin/apply-config-from-env.py conf/proxy.conf && exec bin/pulsar proxy"),
		Env:            PodEnv(),
		EnvFrom:        EnvFrom(p.ProxyName() + "-config"),
		Ports:          ContainerPorts(p.brokerPorts()),
		Resources:      x.Resources,
		ReadinessProbe: tcpProbe(6650, 10, 10, 6),
		LivenessProbe:  tcpProbe(6650, 60, 30, 5),
	}}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: p.ProxyName(), Namespace: p.ns(), Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(x.Replicas),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: PodAnnotations(x.Pod, configHash, x.RestartedAt, Proxy)},
				Spec:       pod,
			},
		},
	}
}

// PDBs returns the disruption budgets of ZooKeeper and the brokers.
func (p Pulsar) PDBs() []*policyv1.PodDisruptionBudget {
	return []*policyv1.PodDisruptionBudget{
		PDB(p.ZooKeeperName(), p.ns(), Labels(p.C.Name, ZooKeeper)),
		PDB(p.BrokerName(), p.ns(), Labels(p.C.Name, Broker)),
	}
}
