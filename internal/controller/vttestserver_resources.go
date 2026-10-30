package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	vtv1 "github.com/likesavabutworse/vttestserver-operator/api/v1alpha1"
)

const (
	// vttestserver ports are derived from --port: gRPC is PORT+1 and MySQL is PORT+3.
	basePort  = 15000
	grpcPort  = basePort + 1
	mysqlPort = basePort + 3

	dataDir      = "/vt/vtdataroot"
	dataVolume   = "data"
	mycnfVolume  = "mycnf"
	tmpVolume    = "tmp"
	pinMountPath = "/etc/vttest/restore-pin"
	cnfMountPath = "/etc/vttest/mysql"
	cnfFileName  = "extra.cnf"

	// vitessUID is the user of the official vttestserver image.
	vitessUID = 999

	datactlBinary = "/datactl"
	argDataDir    = "--data-dir"

	defaultMySQLVersion = "8.4.6-Vitess"
	defaultStorageSize  = "8Gi"

	labelServer = "vttestserver-operator.io/server"
	// annotationMyCnfHash is on the pod template. A new value rolls the pod, which runs the my.cnf init
	// container again. The ConfigMap alone would change the mounted file but not the running mysqld.
	annotationMyCnfHash = "vttestserver-operator.io/mysql-config-hash"
	labelName           = "app.kubernetes.io/name"
	labelInstance       = "app.kubernetes.io/instance"
	labelManaged        = "app.kubernetes.io/managed-by"

	restoreContainer = "restore"
	myCnfContainer   = "apply-my-cnf"
	serverContainer  = "vttestserver"
)

func labelsFor(s *vtv1.VTTestServer) map[string]string {
	return map[string]string{
		labelName:     "vttestserver",
		labelInstance: s.Name,
		labelManaged:  "vttestserver-operator",
		labelServer:   s.Name,
	}
}

func selectorFor(s *vtv1.VTTestServer) map[string]string {
	return map[string]string{labelServer: s.Name}
}

func grpcServiceName(s *vtv1.VTTestServer) string  { return s.Name + "-grpc" }
func cnfConfigMapName(s *vtv1.VTTestServer) string { return s.Name + "-mycnf" }
func pinConfigMapName(s *vtv1.VTTestServer) string { return s.Name + "-restore-pin" }
func podName(s *vtv1.VTTestServer) string          { return s.Name + "-0" }

// pvcName is the PVC that the StatefulSet makes for the first pod.
func pvcName(s *vtv1.VTTestServer) string { return fmt.Sprintf("%s-%s-0", dataVolume, s.Name) }

func mysqlAddress(s *vtv1.VTTestServer) string {
	return fmt.Sprintf("%s.%s.svc:%d", s.Name, s.Namespace, mysqlPort)
}

func grpcAddress(s *vtv1.VTTestServer) string {
	return fmt.Sprintf("%s.%s.svc:%d", grpcServiceName(s), s.Namespace, grpcPort)
}

// serverArgs builds the vttestserver command line. It is the flag list of the
// image run.sh, without --schema-dir, plus persistence. extraArgs come last.
func serverArgs(s *vtv1.VTTestServer) []string {
	names := make([]string, 0, len(s.Spec.Keyspaces))
	shards := make([]string, 0, len(s.Spec.Keyspaces))
	for _, ks := range s.Spec.Keyspaces {
		names = append(names, ks.Name)
		shards = append(shards, strconv.Itoa(int(ks.Shards)))
	}
	args := []string{
		"--port", strconv.Itoa(basePort),
		"--keyspaces", strings.Join(names, ","),
		"--num-shards", strings.Join(shards, ","),
		"--mysql-bind-host", "0.0.0.0",
		"--vtcombo-bind-host", "0.0.0.0",
		"--mysql-server-version", defaultMySQLVersion,
		"--charset", "utf8mb4",
		"--foreign-key-mode", "allow",
		"--enable-online-ddl=true",
		"--enable-direct-ddl=true",
		"--planner-version", "gen4",
		"--tablet-refresh-interval", "10s",
		"--persistent-mode",
		"--data-dir=" + dataDir,
	}
	if s.Spec.AllowVSchemaDDL {
		args = append(args, "--vschema-ddl-authorized-users=%")
	}
	if len(s.Spec.MySQLConfig) > 0 {
		// The EXTRA_MY_CNF environment variable does not work: vttestserver overwrites it from this flag.
		args = append(args, "--extra-my-cnf="+cnfMountPath+"/"+cnfFileName)
	}
	return append(args, s.Spec.ExtraArgs...)
}

// fingerprintArgs are the --keyspace flags of datactl.
func fingerprintArgs(s *vtv1.VTTestServer) []string {
	args := make([]string, 0, 2*len(s.Spec.Keyspaces))
	for _, ks := range s.Spec.Keyspaces {
		args = append(args, "--keyspace", fmt.Sprintf("%s:%d", ks.Name, ks.Shards))
	}
	return args
}

// s3Args are the datactl flags for a store.
func s3Args(s vtv1.S3Spec) []string {
	args := []string{"--bucket", s.Bucket}
	if s.Prefix != "" {
		args = append(args, "--prefix", s.Prefix)
	}
	if s.Region != "" {
		args = append(args, "--region", s.Region)
	}
	if s.Endpoint != "" {
		args = append(args, "--endpoint", s.Endpoint)
	}
	if s.ForcePathStyle {
		args = append(args, "--force-path-style")
	}
	return args
}

func credentialsEnvFrom(s vtv1.S3Spec) []corev1.EnvFromSource {
	if s.CredentialsSecretRef == nil {
		return nil
	}
	return []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: *s.CredentialsSecretRef}}}
}

func myCnf(cfg map[string]string) string {
	keys := make([]string, 0, len(cfg))
	for k := range cfg {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var b strings.Builder
	b.WriteString("[mysqld]\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "%s = %s\n", k, cfg[k])
	}
	return b.String()
}

func myCnfHash(cfg map[string]string) string {
	sum := sha256.Sum256([]byte(myCnf(cfg)))
	return hex.EncodeToString(sum[:8])
}

func buildMyCnf(s *vtv1.VTTestServer) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: cnfConfigMapName(s), Namespace: s.Namespace, Labels: labelsFor(s)},
		Data:       map[string]string{cnfFileName: myCnf(s.Spec.MySQLConfig)},
	}
}

func buildService(s *vtv1.VTTestServer, name, portName string, port int32) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace, Labels: labelsFor(s)},
		Spec: corev1.ServiceSpec{
			Selector: selectorFor(s),
			Ports: []corev1.ServicePort{{
				Name: portName, Port: port, TargetPort: intstr.FromInt32(port), Protocol: corev1.ProtocolTCP,
			}},
		},
	}
}

func restoreInitContainer(s *vtv1.VTTestServer, datactlImage string) corev1.Container {
	rf := s.Spec.RestoreFrom
	args := append([]string{"restore", argDataDir, dataDir}, s3Args(rf.S3)...)
	if rf.Name != "" {
		args = append(args, "--name", rf.Name)
	} else {
		args = append(args, "--tag", rf.Tag, "--pin-file", pinMountPath+"/name")
	}
	args = append(args, fingerprintArgs(s)...)
	return corev1.Container{
		Name:                     restoreContainer,
		Image:                    datactlImage,
		Command:                  []string{datactlBinary},
		Args:                     args,
		Resources:                datactlResources(),
		EnvFrom:                  credentialsEnvFrom(rf.S3),
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		SecurityContext:          containerSecurityContext(),
		VolumeMounts: []corev1.VolumeMount{
			{Name: dataVolume, MountPath: dataDir},
			{Name: "restore-pin", MountPath: pinMountPath, ReadOnly: true},
		},
	}
}

// myCnfInitContainer writes spec.mysqlConfig into the my.cnf of an existing data dir on every pod start.
// Vitess reads --extra-my-cnf only when it first initialises a data dir, so a restored or older data dir
// would keep its old settings.
func myCnfInitContainer(datactlImage string) corev1.Container {
	return corev1.Container{
		Name:            myCnfContainer,
		Image:           datactlImage,
		Command:         []string{datactlBinary},
		Args:            []string{"apply-my-cnf", argDataDir, dataDir, "--file", cnfMountPath + "/" + cnfFileName},
		Resources:       datactlResources(),
		SecurityContext: containerSecurityContext(),
		VolumeMounts: []corev1.VolumeMount{
			{Name: dataVolume, MountPath: dataDir},
			{Name: mycnfVolume, MountPath: cnfMountPath, ReadOnly: true},
		},
	}
}

func containerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: new(false),
		ReadOnlyRootFilesystem:   new(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// podSecurityContext passes the restricted Pod Security profile. The user is the owner of the
// vttestserver data dir.
func podSecurityContext() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsNonRoot:   new(true),
		RunAsUser:      ptr.To[int64](vitessUID),
		RunAsGroup:     ptr.To[int64](vitessUID),
		FSGroup:        ptr.To[int64](vitessUID),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func tcpProbe(port int32, period, failures int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(port)}},
		PeriodSeconds:    period,
		FailureThreshold: failures,
	}
}

// Defaults from a measured run with about 500 MB of data (about 700 MB steady for a restored server with 11 tablets, a data dir of
// about 2.5 to 3 times the data). A value in spec.resources always wins.
var (
	defaultServerMemoryRequest = resource.MustParse("768Mi")
	defaultServerMemoryLimit   = resource.MustParse("1536Mi")
	defaultEphemeralStorage    = resource.MustParse("2Gi")
	defaultDatactlMemRequest   = resource.MustParse("128Mi")
	defaultDatactlMemLimit     = resource.MustParse("256Mi")
)

// datactlResources are the resources of every datactl container: the restore and my.cnf init containers
// and the snapshot and delete Jobs. datactl streams the data, so it needs little memory.
func datactlResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: defaultDatactlMemRequest},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: defaultDatactlMemLimit},
	}
}

// serverResources fills the gaps of spec.resources: a memory request and limit, and for an emptyDir
// data volume an ephemeral-storage request, so that the scheduler counts the node disk. It never
// makes a default request larger than a limit, and never makes a default limit smaller than a request.
func serverResources(s *vtv1.VTTestServer) corev1.ResourceRequirements {
	out := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{},
		Limits:   corev1.ResourceList{},
	}
	for k, v := range s.Spec.Resources.Requests {
		out.Requests[k] = v.DeepCopy()
	}
	for k, v := range s.Spec.Resources.Limits {
		out.Limits[k] = v.DeepCopy()
	}
	req, hasReq := out.Requests[corev1.ResourceMemory]
	lim, hasLim := out.Limits[corev1.ResourceMemory]
	switch {
	case !hasReq && !hasLim:
		out.Requests[corev1.ResourceMemory] = defaultServerMemoryRequest.DeepCopy()
		out.Limits[corev1.ResourceMemory] = defaultServerMemoryLimit.DeepCopy()
	case !hasReq:
		r := defaultServerMemoryRequest.DeepCopy()
		if lim.Cmp(r) < 0 {
			r = lim.DeepCopy()
		}
		out.Requests[corev1.ResourceMemory] = r
	case !hasLim:
		// No default limit above a request that is already larger: a limit below a request is invalid.
		if req.Cmp(defaultServerMemoryLimit) <= 0 {
			out.Limits[corev1.ResourceMemory] = defaultServerMemoryLimit.DeepCopy()
		}
	}
	if s.Spec.Storage.Type != vtv1.StoragePersistent {
		if _, ok := out.Requests[corev1.ResourceEphemeralStorage]; !ok {
			out.Requests[corev1.ResourceEphemeralStorage] = defaultEphemeralStorage.DeepCopy()
		}
	}
	if len(out.Limits) == 0 {
		out.Limits = nil
	}
	return out
}

func podAnnotations(s *vtv1.VTTestServer) map[string]string {
	if len(s.Spec.MySQLConfig) == 0 {
		return nil
	}
	return map[string]string{annotationMyCnfHash: myCnfHash(s.Spec.MySQLConfig)}
}

func buildStatefulSet(s *vtv1.VTTestServer, datactlImage string, replicas int32) *appsv1.StatefulSet {
	labels := labelsFor(s)
	mounts := []corev1.VolumeMount{{Name: dataVolume, MountPath: dataDir}, {Name: tmpVolume, MountPath: "/tmp"}}
	volumes := []corev1.Volume{{Name: tmpVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}

	if len(s.Spec.MySQLConfig) > 0 {
		volumes = append(volumes, corev1.Volume{Name: mycnfVolume, VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: cnfConfigMapName(s)}},
		}})
		mounts = append(mounts, corev1.VolumeMount{Name: mycnfVolume, MountPath: cnfMountPath, ReadOnly: true})
	}

	var initContainers []corev1.Container
	if s.Spec.RestoreFrom != nil {
		initContainers = append(initContainers, restoreInitContainer(s, datactlImage))
		volumes = append(volumes, corev1.Volume{Name: "restore-pin", VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: pinConfigMapName(s)},
				Optional:             new(true),
			},
		}})
	}

	if len(s.Spec.MySQLConfig) > 0 {
		// After the restore: it changes the my.cnf that the restore brought.
		initContainers = append(initContainers, myCnfInitContainer(datactlImage))
	}

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: s.Name, Namespace: s.Namespace, Labels: labels},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    new(replicas),
			ServiceName: s.Name,
			Selector:    &metav1.LabelSelector{MatchLabels: selectorFor(s)},
			// A snapshot scales to 0 and needs the volume afterwards. Deleting the server removes it.
			PersistentVolumeClaimRetentionPolicy: &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenDeleted: appsv1.DeletePersistentVolumeClaimRetentionPolicyType,
				WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: podAnnotations(s)},
				Spec: corev1.PodSpec{
					ServiceAccountName: s.Spec.ServiceAccountName,
					SecurityContext:    podSecurityContext(),
					InitContainers:     initContainers,
					Containers: []corev1.Container{{
						Name:      serverContainer,
						Image:     s.Spec.Image,
						Command:   []string{"/vt/bin/vttestserver"},
						Args:      serverArgs(s),
						Resources: serverResources(s),
						Ports: []corev1.ContainerPort{
							{Name: "grpc", ContainerPort: grpcPort},
							{Name: "mysql", ContainerPort: mysqlPort},
						},
						// The first start of a large data set can take minutes.
						StartupProbe:    tcpProbe(mysqlPort, 5, 120),
						ReadinessProbe:  tcpProbe(mysqlPort, 5, 3),
						SecurityContext: containerSecurityContext(),
						VolumeMounts:    mounts,
					}},
					Volumes: volumes,
				},
			},
		},
	}

	if s.Spec.Storage.Type == vtv1.StoragePersistent {
		size := resource.MustParse(defaultStorageSize)
		if s.Spec.Storage.Size != nil {
			size = *s.Spec.Storage.Size
		}
		sts.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{
			ObjectMeta: metav1.ObjectMeta{Name: dataVolume, Labels: labels},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				StorageClassName: s.Spec.Storage.StorageClassName,
				Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size}},
			},
		}}
	} else {
		sts.Spec.Template.Spec.Volumes = append(sts.Spec.Template.Spec.Volumes, corev1.Volume{
			Name: dataVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
	}
	return sts
}
