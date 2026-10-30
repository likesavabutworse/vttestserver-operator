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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Phase values of a VTTestServerSnapshot.
const (
	SnapshotPending   = "Pending"
	SnapshotQuiescing = "Quiescing"
	SnapshotUploading = "Uploading"
	SnapshotReady     = "Ready"
	SnapshotFailed    = "Failed"
	SnapshotExpired   = "Expired"
	SnapshotDeleting  = "Deleting"
)

// deletionPolicy values.
const (
	DeletionRetain = "Retain"
	DeletionDelete = "Delete"
)

// consistency values.
const (
	ConsistencyClean = "clean"
)

// Condition types of a VTTestServerSnapshot.
const (
	ConditionSnapshot = "Complete"
)

const (
	// SnapshotFinalizer blocks deletion until the operator has deleted the S3 objects. Only added when
	// deletionPolicy is Delete; a Retain snapshot never gets it.
	SnapshotFinalizer = "vttestserver-operator.io/snapshot-cleanup"
	// SnapshotHoldAnnot is set on the source VTTestServer with the holding snapshot's name, so the server
	// stays stopped (and reports Suspended) for that one snapshot and is not released by another.
	SnapshotHoldAnnot = "vttestserver-operator.io/snapshot-hold"
)

// ServerRef names a VTTestServer in the namespace of the snapshot.
type ServerRef struct {
	// name is the VTTestServer name. The server needs persistent storage.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// VTTestServerSnapshotSpec defines one snapshot request. It cannot change after creation.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
type VTTestServerSnapshotSpec struct {
	// serverRef is the server to snapshot.
	ServerRef ServerRef `json:"serverRef"`

	// serviceAccountName is used by the snapshot Job. IRSA or Pod Identity needs it.
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`

	// s3 is the snapshot store.
	S3 S3Spec `json:"s3"`

	// tags are pointers to this snapshot, for example latest.
	// +optional
	Tags []string `json:"tags,omitempty"`

	// consistency is clean: the server stops before the copy.
	// +kubebuilder:validation:Enum=clean
	// +kubebuilder:default=clean
	Consistency string `json:"consistency,omitempty"`

	// ttl is how long the snapshot lives. After that, a Job deletes the S3 objects.
	// +optional
	TTL *metav1.Duration `json:"ttl,omitempty"`

	// deletionPolicy says what happens to the S3 objects when this object is deleted.
	// +kubebuilder:validation:Enum=Retain;Delete
	// +kubebuilder:default=Retain
	DeletionPolicy string `json:"deletionPolicy,omitempty"`
}

// SnapshotFingerprint is the topology that the snapshot has.
type SnapshotFingerprint struct {
	Keyspaces []KeyspaceSpec `json:"keyspaces,omitempty"`
}

// VTTestServerSnapshotStatus defines the observed state of VTTestServerSnapshot.
type VTTestServerSnapshotStatus struct {
	// phase is the lifecycle phase.
	// +optional
	Phase string `json:"phase,omitempty"`

	// phaseStartedAt is when the phase began. The deadlines of Quiescing and Uploading count from it.
	// +optional
	PhaseStartedAt *metav1.Time `json:"phaseStartedAt,omitempty"`

	// jobName is the Job that uploads or deletes the snapshot.
	// +optional
	JobName string `json:"jobName,omitempty"`

	// completedAt is when the upload finished.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// sizeBytes is the size of the archive in the store.
	// +optional
	SizeBytes int64 `json:"sizeBytes,omitempty"`

	// sha256 is the digest of the archive.
	// +optional
	SHA256 string `json:"sha256,omitempty"`

	// s3Uri is where the snapshot lives.
	// +optional
	S3URI string `json:"s3Uri,omitempty"`

	// fingerprint is the topology of the source server.
	// +optional
	Fingerprint *SnapshotFingerprint `json:"fingerprint,omitempty"`

	// message has the error text of a failure.
	// +optional
	Message string `json:"message,omitempty"`

	// conditions represent the current state of the snapshot.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Server",type=string,JSONPath=`.spec.serverRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Size",type=integer,JSONPath=`.status.sizeBytes`
// +kubebuilder:printcolumn:name="Completed",type=date,JSONPath=`.status.completedAt`

// VTTestServerSnapshot is a request for one snapshot of a VTTestServer, with its status.
type VTTestServerSnapshot struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of VTTestServerSnapshot
	// +required
	Spec VTTestServerSnapshotSpec `json:"spec"`

	// status defines the observed state of VTTestServerSnapshot
	// +optional
	Status VTTestServerSnapshotStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// VTTestServerSnapshotList contains a list of VTTestServerSnapshot
type VTTestServerSnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []VTTestServerSnapshot `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &VTTestServerSnapshot{}, &VTTestServerSnapshotList{})
		return nil
	})
}
