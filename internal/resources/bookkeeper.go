package resources

import (
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	v1 "github.com/SongOf/pulsar-operator/api/v1alpha1"
)

// Bookie volume names, which are also the prefixes of their PVC names.
const (
	JournalVolume = "journal"
	LedgerVolume  = "ledgers"
	journalPath   = "/pulsar/data/bookkeeper/journal"
	ledgerPath    = "/pulsar/data/bookkeeper/ledgers"
	BookiePort    = 3181
)

// BookKeeper builds the objects of a BookKeeperCluster.
type BookKeeper struct{ C *v1.BookKeeperCluster }

func (b BookKeeper) name(component string) string { return Name(b.C.Name, component) }

// StatefulSetName is the bookie StatefulSet, also the name of its headless Service.
func (b BookKeeper) StatefulSetName() string { return b.name(Bookie) }

// PodName is the bookie pod with an ordinal.
func (b BookKeeper) PodName(ordinal int32) string {
	return fmt.Sprintf("%s-%d", b.StatefulSetName(), ordinal)
}

// BookieID is the identity a bookie registers with: its advertised FQDN and port.
func (b BookKeeper) BookieID(ordinal int32) string {
	return fmt.Sprintf("%s:%d", PodFQDN(b.PodName(ordinal), b.StatefulSetName(), b.C.Namespace, b.C.Spec.ClusterDomain), BookiePort)
}

// PVCName is the claim of a bookie volume.
func (b BookKeeper) PVCName(volume string, ordinal int32) string {
	return fmt.Sprintf("%s-%s", volume, b.PodName(ordinal))
}

// MetadataServiceURI is the BookKeeper metadata URI; BookKeeper separates ZooKeeper hosts with semicolons.
func (b BookKeeper) MetadataServiceURI() string {
	root := b.C.Spec.LedgersRootPath
	if root == "" {
		root = "/ledgers"
	}
	return fmt.Sprintf("zk+hierarchical://%s%s", strings.ReplaceAll(b.C.Spec.ZooKeeper, ",", ";"), root)
}

func (b BookKeeper) image() string { return ImageOf(b.C.Spec.Image, "") }

// ConfigMapName is the environment shared by bookies, auto-recovery and the BookKeeper jobs.
func (b BookKeeper) ConfigMapName() string { return b.name(Bookie) + "-config" }

// ConfigMap is the bookies' configuration.
func (b BookKeeper) ConfigMap() *corev1.ConfigMap {
	s := b.C.Spec
	defaults := map[string]string{
		"metadataServiceUri":    b.MetadataServiceURI(),
		"zkServers":             s.ZooKeeper,
		"journalDirectories":    journalPath,
		"ledgerDirectories":     ledgerPath,
		"httpServerEnabled":     "true",
		"httpServerPort":        "8000",
		"bookiePort":            fmt.Sprint(BookiePort),
		"useHostNameAsBookieID": "true",
	}
	owned := []string{"metadataServiceUri", "journalDirectories", "ledgerDirectories", "httpServerEnabled", "httpServerPort", "bookiePort", "useHostNameAsBookieID"}
	mem := JVMOptions(s.JVM, "1g", "1g")
	env := map[string]string{"PULSAR_MEM": mem, "BOOKIE_MEM": mem}
	return ConfigMap(b.ConfigMapName(), b.C.Namespace, Labels(b.C.Name, Bookie), ConfigData(defaults, s.Config, owned, env))
}

func (b BookKeeper) ports() []corev1.ServicePort {
	return []corev1.ServicePort{Port("bookie", BookiePort), Port("http", 8000)}
}

// HeadlessService gives bookies their stable FQDNs, which are their IDs.
func (b BookKeeper) HeadlessService() *corev1.Service {
	return HeadlessService(b.StatefulSetName(), b.C.Namespace, Labels(b.C.Name, Bookie), b.ports())
}

// VolumeClaimTemplates are the journal and ledger PVCs of each bookie.
func (b BookKeeper) VolumeClaimTemplates() []corev1.PersistentVolumeClaim {
	st := b.C.Spec.Storage
	claim := func(name string, v v1.VolumeSpec) corev1.PersistentVolumeClaim {
		return corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: Labels(b.C.Name, Bookie)},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				StorageClassName: v.StorageClassName,
				Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: v.Size}},
			},
		}
	}
	return []corev1.PersistentVolumeClaim{claim(JournalVolume, st.Journal), claim(LedgerVolume, st.Ledger)}
}

// VolumeTemplateHash identifies the volume claim templates; they are immutable on a StatefulSet, so a change
// means the StatefulSet is recreated with its pods orphaned.
func (b BookKeeper) VolumeTemplateHash() string {
	st := b.C.Spec.Storage
	sc := func(v v1.VolumeSpec) string { return ptr.Deref(v.StorageClassName, "") }
	return Hash(map[string]string{
		"journal": st.Journal.Size.String(), "journalClass": sc(st.Journal),
		"ledger": st.Ledger.Size.String(), "ledgerClass": sc(st.Ledger),
	})
}

// AnnotationVolumeTemplateHash records VolumeTemplateHash on the StatefulSet.
const AnnotationVolumeTemplateHash = "pulsar.songof.dev/volume-template-hash"

// StatefulSet runs the bookies. Its replicas are set by the controller, which shrinks it one bookie at a time.
func (b BookKeeper) StatefulSet(replicas int32, configHash string) *appsv1.StatefulSet {
	s := b.C.Spec
	labels := Labels(b.C.Name, Bookie)
	pod := PodSpec(s.Pod, labels, 60)
	reclaim := appsv1.RetainPersistentVolumeClaimRetentionPolicyType
	if s.Storage.ReclaimPolicy == v1.ReclaimDelete {
		reclaim = appsv1.DeletePersistentVolumeClaimRetentionPolicyType
	}
	fqdn := fmt.Sprintf("$(POD_NAME).%s.$(POD_NAMESPACE).svc.%s", b.StatefulSetName(), domainOr(s.ClusterDomain))
	pod.Containers = []corev1.Container{{
		Name:            Bookie,
		Image:           b.image(),
		Command:         Shell("bin/apply-config-from-env.py conf/bookkeeper.conf && exec bin/pulsar bookie"),
		Env:             append(PodEnv(), corev1.EnvVar{Name: "PULSAR_PREFIX_advertisedAddress", Value: fqdn}),
		EnvFrom:         EnvFrom(b.ConfigMapName()),
		Ports:           ContainerPorts(b.ports()),
		Resources:       s.Resources,
		ReadinessProbe:  httpProbe("/api/v1/bookie/is_ready", 8000, 10, 10, 6),
		LivenessProbe:   tcpProbe(BookiePort, 60, 30, 5),
		VolumeMounts: []corev1.VolumeMount{
			{Name: JournalVolume, MountPath: journalPath},
			{Name: LedgerVolume, MountPath: ledgerPath},
		},
	}}
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: b.StatefulSetName(), Namespace: b.C.Namespace, Labels: labels,
			Annotations: map[string]string{AnnotationVolumeTemplateHash: b.VolumeTemplateHash()},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:            ptr.To(replicas),
			ServiceName:         b.StatefulSetName(),
			Selector:            &metav1.LabelSelector{MatchLabels: labels},
			PodManagementPolicy: appsv1.ParallelPodManagement,
			UpdateStrategy:      appsv1.StatefulSetUpdateStrategy{Type: appsv1.RollingUpdateStatefulSetStrategyType},
			PersistentVolumeClaimRetentionPolicy: &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenDeleted: reclaim,
				// Scale-in keeps the PVCs: the controller deletes them only after the bookie is decommissioned.
				WhenScaled: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: PodAnnotations(s.Pod, configHash, s.RestartedAt, Bookie)},
				Spec:       pod,
			},
			VolumeClaimTemplates: b.VolumeClaimTemplates(),
		},
	}
}

// AutoRecoveryEnabled reports whether auto-recovery runs, which is the default.
func (b BookKeeper) AutoRecoveryEnabled() bool {
	return ptr.Deref(b.C.Spec.AutoRecovery.Enabled, true)
}

// AutoRecovery runs the auditor and replication workers that re-replicate ledgers of missing bookies.
func (b BookKeeper) AutoRecovery(configHash string) *appsv1.Deployment {
	ar := b.C.Spec.AutoRecovery
	labels := Labels(b.C.Name, AutoRecovery)
	pod := PodSpec(ar.Pod, labels, 30)
	pod.Containers = []corev1.Container{{
		Name:      AutoRecovery,
		Image:     ImageOf(ar.Image, b.image()),
		Command:   Shell("bin/apply-config-from-env.py conf/bookkeeper.conf && exec bin/bookkeeper autorecovery"),
		Env:       append(PodEnv(), corev1.EnvVar{Name: "BOOKIE_MEM", Value: JVMOptions(ar.JVM, "256m", "256m")}),
		EnvFrom:   EnvFrom(b.ConfigMapName()),
		Resources: ar.Resources,
	}}
	replicas := ar.Replicas
	if replicas == 0 {
		replicas = 1
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: b.name(AutoRecovery), Namespace: b.C.Namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(replicas),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: PodAnnotations(ar.Pod, configHash, ar.RestartedAt)},
				Spec:       pod,
			},
		},
	}
}

// AutoRecoveryName is the auto-recovery Deployment.
func (b BookKeeper) AutoRecoveryName() string { return b.name(AutoRecovery) }

// PodDisruptionBudget lets voluntary disruptions take at most one bookie at a time.
func (b BookKeeper) PodDisruptionBudget() *policyv1.PodDisruptionBudget {
	return PDB(b.StatefulSetName(), b.C.Namespace, Labels(b.C.Name, Bookie))
}

// InitJobName formats the BookKeeper metadata.
func (b BookKeeper) InitJobName() string { return b.name(Bookie) + "-init" }

// InitJob formats the BookKeeper metadata unless it exists, so it is safe to run against an initialised cluster.
func (b BookKeeper) InitJob() *batchv1.Job {
	return b.job(b.InitJobName(), "bin/apply-config-from-env.py conf/bookkeeper.conf >/dev/null && "+
		"if bin/bookkeeper shell whatisinstanceid; then echo 'BookKeeper metadata already exists'; "+
		"else bin/bookkeeper shell initnewcluster; fi", 6, 0)
}

// DecommissionJobName is the job removing the bookie with an ordinal.
func (b BookKeeper) DecommissionJobName(ordinal int32) string {
	return fmt.Sprintf("%s-decommission-%d", b.name(Bookie), ordinal)
}

// DecommissionJob waits until every ledger of the stopped bookie is re-replicated, then removes its cookie.
func (b BookKeeper) DecommissionJob(ordinal int32) *batchv1.Job {
	return b.job(b.DecommissionJobName(ordinal), "bin/apply-config-from-env.py conf/bookkeeper.conf >/dev/null && "+
		"exec bin/bookkeeper shell decommissionbookie -bookieid "+b.BookieID(ordinal), 3, 6*3600)
}

func (b BookKeeper) job(name, script string, backoff int32, deadline int64) *batchv1.Job {
	labels := Labels(b.C.Name, Bookie+"-job")
	pod := PodSpec(v1.PodPolicy{NodeSelector: b.C.Spec.Pod.NodeSelector, Tolerations: b.C.Spec.Pod.Tolerations, AntiAffinity: v1.AntiAffinityNone}, labels, 30)
	pod.RestartPolicy = corev1.RestartPolicyOnFailure
	pod.Containers = []corev1.Container{{
		Name:    "job",
		Image:   b.image(),
		Command: Shell(script),
		Env:     append(PodEnv(), corev1.EnvVar{Name: "BOOKIE_MEM", Value: "-Xms256m -Xmx256m"}),
		EnvFrom: EnvFrom(b.ConfigMapName()),
	}}
	spec := batchv1.JobSpec{
		BackoffLimit: ptr.To(backoff),
		Template:     corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: pod},
	}
	if deadline > 0 {
		spec.ActiveDeadlineSeconds = ptr.To(deadline)
	}
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: b.C.Namespace, Labels: labels}, Spec: spec}
}

// PDB allows one pod of a component to be disrupted at a time.
func PDB(name, namespace string, labels map[string]string) *policyv1.PodDisruptionBudget {
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: ptr.To(intstr.FromInt32(1)),
			Selector:       &metav1.LabelSelector{MatchLabels: labels},
		},
	}
}

func httpProbe(path string, port int32, delay, period, failures int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: path, Port: intstr.FromInt32(port)}},
		InitialDelaySeconds: delay, PeriodSeconds: period, FailureThreshold: failures, TimeoutSeconds: 5,
	}
}

func tcpProbe(port int32, delay, period, failures int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler:        corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(port)}},
		InitialDelaySeconds: delay, PeriodSeconds: period, FailureThreshold: failures, TimeoutSeconds: 5,
	}
}
