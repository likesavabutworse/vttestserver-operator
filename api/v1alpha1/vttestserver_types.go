/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Phase values of a VTTestServer.
const (
	PhasePending     = "Pending"
	PhaseRestoring   = "Restoring"
	PhaseStarting    = "Starting"
	PhaseConfiguring = "Configuring"
	PhaseReady       = "Ready"
	PhaseSuspended   = "Suspended"
	PhaseFailed      = "Failed"
)

// Condition types of a VTTestServer.
const (
	ConditionRestored       = "Restored"
	ConditionVSchemaApplied = "VSchemaApplied"
	ConditionInitSQLApplied = "InitSQLApplied"
	ConditionReady          = "Ready"
	ReasonVSchemaMissing    = "VSchemaMissing"
	ReasonRestoreFailed     = "RestoreFailed"
)

// KeyspaceSpec is one keyspace of the server.
type KeyspaceSpec struct {
	// name is the keyspace name.
	// +kubebuilder:validation:Pattern=`^[a-zA-Z][a-zA-Z0-9_]*$`
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// shards is the shard count. It is the vttestserver --num-shards flag, not part of the VSchema.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=256
	// +kubebuilder:default=1
	Shards int32 `json:"shards"`
}

// StorageType is the kind of volume under the data dir.
// +kubebuilder:validation:Enum=persistent;ephemeral
type StorageType string

const (
	StoragePersistent StorageType = "persistent"
	StorageEphemeral  StorageType = "ephemeral"
)

// StorageSpec describes the volume mounted at the data dir /vt/vtdataroot.
type StorageSpec struct {
	// type is persistent (a PVC, needed as a snapshot source) or ephemeral (an emptyDir).
	// +kubebuilder:default=ephemeral
	Type StorageType `json:"type,omitempty"`

	// size is the PVC size. It is used only for persistent storage.
	// +optional
	Size *resource.Quantity `json:"size,omitempty"`

	// storageClassName is the storage class of the PVC. The cluster default is used when empty.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`
}

// ConfigMapKeyRef selects one key of a ConfigMap in the namespace of the server.
type ConfigMapKeyRef struct {
	// name is the ConfigMap name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// key is the key inside the ConfigMap.
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}

// VSchemaSource points at the ConfigMap with one JSON key per keyspace.
type VSchemaSource struct {
	// configMapRef names a ConfigMap with the keys <keyspace>.json. A keyspace without a key gets no VSchema.
	ConfigMapRef corev1.LocalObjectReference `json:"configMapRef"`
}

// InitSQLSource is one SQL file that the operator runs in a keyspace.
type InitSQLSource struct {
	// keyspace is where the statements run. It must be one of spec.keyspaces.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Keyspace string `json:"keyspace"`
	// configMapKeyRef points at the SQL text. The statements must be idempotent.
	ConfigMapKeyRef ConfigMapKeyRef `json:"configMapKeyRef"`
}

// S3Spec selects S3 or S3 compatible storage for snapshots.
type S3Spec struct {
	// bucket is the bucket name.
	// +kubebuilder:validation:MinLength=1
	Bucket string `json:"bucket"`
	// prefix is the key prefix inside the bucket.
	// +optional
	Prefix string `json:"prefix,omitempty"`
	// region is the AWS region.
	// +optional
	Region string `json:"region,omitempty"`
	// endpoint is the URL of S3 compatible storage, for example in-cluster Silo or MinIO.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// forcePathStyle turns on path style addressing, which most S3 compatible storage needs.
	// +optional
	ForcePathStyle bool `json:"forcePathStyle,omitempty"`
	// credentialsSecretRef names a Secret with AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY and
	// optional AWS_SESSION_TOKEN. Leave it empty to use IRSA or Pod Identity through serviceAccountName.
	// +optional
	CredentialsSecretRef *corev1.LocalObjectReference `json:"credentialsSecretRef,omitempty"`
}

// RestoreSpec selects the snapshot to restore. Set exactly one of name and tag.
// +kubebuilder:validation:XValidation:rule="has(self.name) != has(self.tag)",message="set exactly one of name and tag"
type RestoreSpec struct {
	// name is the snapshot name.
	// +optional
	Name string `json:"name,omitempty"`
	// tag is a tag such as latest. The first restore pins the snapshot that the tag points to.
	// +optional
	Tag string `json:"tag,omitempty"`
	// s3 is the snapshot store.
	S3 S3Spec `json:"s3"`
}

// VTTestServerSpec defines the desired state of VTTestServer.
// +kubebuilder:validation:XValidation:rule="has(self.restoreFrom) == has(oldSelf.restoreFrom) && (!has(self.restoreFrom) || self.restoreFrom == oldSelf.restoreFrom)",message="restoreFrom is immutable"
// +kubebuilder:validation:XValidation:rule="self.keyspaces.all(k, k.shards == 1) || has(self.vschema) || has(self.restoreFrom)",message="a keyspace with more than one shard needs spec.vschema or spec.restoreFrom"
// +kubebuilder:validation:XValidation:rule="!has(self.initSQL) || self.initSQL.all(s, self.keyspaces.exists(k, k.name == s.keyspace))",message="initSQL keyspace must be one of spec.keyspaces"
type VTTestServerSpec struct {
	// image is the vttestserver image. Pin it by digest. The official vitess/vttestserver image works.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// keyspaces lists the keyspaces and their shard counts. It cannot change after creation.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="keyspaces is immutable"
	// +listType=map
	// +listMapKey=name
	Keyspaces []KeyspaceSpec `json:"keyspaces"`

	// resources are the container resources of vttestserver.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// storage is the data volume. The default is an emptyDir.
	// +optional
	Storage StorageSpec `json:"storage,omitempty"`

	// mysqlConfig adds my.cnf settings, for example innodb_buffer_pool_size: 256M.
	// +optional
	MySQLConfig map[string]string `json:"mysqlConfig,omitempty"`

	// extraArgs are added at the end of the vttestserver command line.
	// +optional
	ExtraArgs []string `json:"extraArgs,omitempty"`

	// allowVSchemaDDL turns on ALTER VSCHEMA. A later apply of spec.vschema overwrites such changes.
	// +optional
	AllowVSchemaDDL bool `json:"allowVSchemaDDL,omitempty"`

	// vschema is the ConfigMap with the VSchema JSON per keyspace.
	// +optional
	VSchema *VSchemaSource `json:"vschema,omitempty"`

	// initSQL is an ordered list of SQL files. The operator runs them after the VSchema.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	InitSQL []InitSQLSource `json:"initSQL,omitempty"`

	// serviceAccountName is used by the pod. A restore with IRSA or Pod Identity needs it.
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`

	// restoreFrom restores a snapshot into an empty data volume, once. It cannot change after creation.
	// +optional
	RestoreFrom *RestoreSpec `json:"restoreFrom,omitempty"`
}

// Endpoints are the in-cluster addresses of the server.
type Endpoints struct {
	// mysql is the <name>.<namespace>.svc:<port> address of the vtgate MySQL port.
	MySQL string `json:"mysql,omitempty"`
	// grpc is the <name>.<namespace>.svc:<port> address of the vtctld gRPC port.
	GRPC string `json:"grpc,omitempty"`
}

// RestoredFrom records the snapshot that a restore used.
type RestoredFrom struct {
	// name is the snapshot name, resolved from spec.restoreFrom.tag or .name.
	Name string `json:"name"`
	// sha256 is the digest of the restored archive.
	SHA256 string `json:"sha256,omitempty"`
	// resolvedAt is when the restore finished.
	ResolvedAt metav1.Time `json:"resolvedAt,omitempty"`
}

// AppliedState records a hash of what the operator applied.
type AppliedState struct {
	// appliedHash is a hash of the VSchema or init SQL that was last applied.
	AppliedHash string `json:"appliedHash,omitempty"`
	// appliedAt is when it was applied.
	AppliedAt *metav1.Time `json:"appliedAt,omitempty"`
}

// VTTestServerStatus defines the observed state of VTTestServer.
type VTTestServerStatus struct {
	// phase is the lifecycle phase.
	// +optional
	Phase string `json:"phase,omitempty"`

	// observedGeneration is the spec generation that the status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// endpoints are the in-cluster addresses.
	// +optional
	Endpoints Endpoints `json:"endpoints,omitempty"`

	// restoredFrom is the snapshot that the restore pinned.
	// +optional
	RestoredFrom *RestoredFrom `json:"restoredFrom,omitempty"`

	// vschema records the applied VSchema hash.
	// +optional
	VSchema AppliedState `json:"vschema,omitempty"`

	// initSQL records the applied init SQL hash.
	// +optional
	InitSQL AppliedState `json:"initSQL,omitempty"`

	// appliedPodUID is the pod that the VSchema and init SQL were applied to. A new pod gets them again.
	// +optional
	AppliedPodUID string `json:"appliedPodUID,omitempty"`

	// conditions are Restored, VSchemaApplied, InitSQLApplied and Ready.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="MySQL",type=string,JSONPath=`.status.endpoints.mysql`
// +kubebuilder:printcolumn:name="Restored",type=string,JSONPath=`.status.restoredFrom.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// VTTestServer is one ephemeral vttestserver instance.
type VTTestServer struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of VTTestServer
	// +required
	Spec VTTestServerSpec `json:"spec"`

	// status defines the observed state of VTTestServer
	// +optional
	Status VTTestServerStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// VTTestServerList contains a list of VTTestServer
type VTTestServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []VTTestServer `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &VTTestServer{}, &VTTestServerList{})
		return nil
	})
}
