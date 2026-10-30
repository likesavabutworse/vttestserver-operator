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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	vtv1 "github.com/likesavabutworse/vttestserver-operator/api/v1alpha1"
	"github.com/likesavabutworse/vttestserver-operator/internal/snapshot/manifest"
)

const (
	pollInterval = 3 * time.Second
	jobLabel     = "vttestserver-operator.io/snapshot"
)

// VTTestServerSnapshotReconciler reconciles a VTTestServerSnapshot object
type VTTestServerSnapshotReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	// DatactlImage is the image of the snapshot and delete Jobs.
	DatactlImage string
	// MaxConcurrentReconciles is the number of snapshots reconciled at the same time. 1 when 0.
	MaxConcurrentReconciles int
	// QuiesceTimeout is how long a snapshot waits for the server to stop. 10 minutes when 0.
	QuiesceTimeout time.Duration
	// UploadTimeout is how long the upload Job may run. 30 minutes when 0.
	UploadTimeout time.Duration
}

// +kubebuilder:rbac:groups=vttestserver-operator.io,resources=vttestserversnapshots,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=vttestserver-operator.io,resources=vttestserversnapshots/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=vttestserver-operator.io,resources=vttestserversnapshots/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete

func (r *VTTestServerSnapshotReconciler) quiesceTimeout() time.Duration {
	if r.QuiesceTimeout > 0 {
		return r.QuiesceTimeout
	}
	return 10 * time.Minute
}

func (r *VTTestServerSnapshotReconciler) uploadTimeout() time.Duration {
	if r.UploadTimeout > 0 {
		return r.UploadTimeout
	}
	return 30 * time.Minute
}

func (r *VTTestServerSnapshotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var snap vtv1.VTTestServerSnapshot
	if err := r.Get(ctx, req.NamespacedName, &snap); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	orig := snap.DeepCopy()
	var res ctrl.Result
	var err error
	if !snap.DeletionTimestamp.IsZero() {
		res, err = r.finalize(ctx, &snap)
	} else {
		res, err = r.reconcile(ctx, &snap)
	}
	if apierrors.IsConflict(err) {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if orig.Status.Phase != snap.Status.Phase {
		now := metav1.Now()
		snap.Status.PhaseStartedAt = &now
		logf.FromContext(ctx).Info("phase changed", "from", orig.Status.Phase, "to", snap.Status.Phase, "message", snap.Status.Message)
	}
	if !equality.Semantic.DeepEqual(orig.Status, snap.Status) {
		// The lock keeps a pass that started from a stale cached copy from writing an old phase over a newer one.
		if perr := r.Status().Patch(ctx, &snap, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); perr != nil {
			if apierrors.IsConflict(perr) {
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			if err == nil {
				err = perr
			}
		}
	}
	return res, err
}

func (r *VTTestServerSnapshotReconciler) reconcile(ctx context.Context, snap *vtv1.VTTestServerSnapshot) (ctrl.Result, error) {
	// The finalizer exists only when the S3 objects must go with the object.
	if snap.Spec.DeletionPolicy == vtv1.DeletionDelete && controllerutil.AddFinalizer(snap, vtv1.SnapshotFinalizer) {
		if err := r.Update(ctx, snap); err != nil {
			return ctrl.Result{}, err
		}
	}
	switch snap.Status.Phase {
	case vtv1.SnapshotReady:
		// The result is in the status, so the Job can go. A finished Job pod that stays would keep the
		// volume claim of the server in use: deleting the server would leave the claim stuck in Terminating.
		if err := r.deleteUploadJob(ctx, snap); err != nil {
			return ctrl.Result{}, err
		}
		return r.checkExpiry(ctx, snap)
	case vtv1.SnapshotFailed, vtv1.SnapshotExpired:
		return ctrl.Result{}, nil
	case vtv1.SnapshotDeleting:
		return r.expire(ctx, snap)
	}

	var server vtv1.VTTestServer
	err := r.Get(ctx, client.ObjectKey{Namespace: snap.Namespace, Name: snap.Spec.ServerRef.Name}, &server)
	if apierrors.IsNotFound(err) {
		if snap.Status.Phase == vtv1.SnapshotQuiescing || snap.Status.Phase == vtv1.SnapshotUploading {
			return r.failSnapshot(ctx, snap, nil, fmt.Sprintf("server %q was deleted during the snapshot", snap.Spec.ServerRef.Name))
		}
		snap.Status.Phase = vtv1.SnapshotPending
		snap.Status.Message = fmt.Sprintf("server %q does not exist", snap.Spec.ServerRef.Name)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if server.Spec.Storage.Type != vtv1.StoragePersistent {
		return r.failSnapshot(ctx, snap, nil, "the server must use persistent storage: an ephemeral volume cannot be a snapshot source")
	}

	switch snap.Status.Phase {
	case "", vtv1.SnapshotPending:
		return r.start(ctx, snap, &server)
	case vtv1.SnapshotQuiescing:
		return r.quiesce(ctx, snap, &server)
	case vtv1.SnapshotUploading:
		return r.upload(ctx, snap, &server)
	}
	return ctrl.Result{}, nil
}

// start takes the hold on the server, so that its controller stops the pod.
func (r *VTTestServerSnapshotReconciler) start(ctx context.Context, snap *vtv1.VTTestServerSnapshot, server *vtv1.VTTestServer) (ctrl.Result, error) {
	if holder := server.Annotations[vtv1.SnapshotHoldAnnot]; holder != "" && holder != snap.Name {
		snap.Status.Phase = vtv1.SnapshotPending
		snap.Status.Message = fmt.Sprintf("server is held by snapshot %q", holder)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if server.Status.Phase != vtv1.PhaseReady && server.Annotations[vtv1.SnapshotHoldAnnot] == "" {
		snap.Status.Phase = vtv1.SnapshotPending
		snap.Status.Message = fmt.Sprintf("server is %q, waiting for Ready", server.Status.Phase)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	if server.Annotations[vtv1.SnapshotHoldAnnot] != snap.Name {
		// The optimistic lock makes two snapshots of one server unable to both take the hold.
		patch := client.MergeFromWithOptions(server.DeepCopy(), client.MergeFromWithOptimisticLock{})
		if server.Annotations == nil {
			server.Annotations = map[string]string{}
		}
		server.Annotations[vtv1.SnapshotHoldAnnot] = snap.Name
		if err := r.Patch(ctx, server, patch); err != nil {
			return ctrl.Result{}, err
		}
	}
	snap.Status.Phase = vtv1.SnapshotQuiescing
	snap.Status.Message = "stopping the server"
	return ctrl.Result{RequeueAfter: pollInterval}, nil
}

// quiesce waits until the server pod is gone, then starts the upload Job.
func (r *VTTestServerSnapshotReconciler) quiesce(ctx context.Context, snap *vtv1.VTTestServerSnapshot, server *vtv1.VTTestServer) (ctrl.Result, error) {
	if started := snap.Status.PhaseStartedAt; started != nil && time.Since(started.Time) > r.quiesceTimeout() {
		return r.failSnapshot(ctx, snap, server, fmt.Sprintf("the server did not stop within %s", r.quiesceTimeout()))
	}
	var pod corev1.Pod
	err := r.Get(ctx, client.ObjectKey{Namespace: server.Namespace, Name: podName(server)}, &pod)
	if err == nil {
		return ctrl.Result{RequeueAfter: pollInterval}, nil
	}
	if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	var sts appsv1.StatefulSet
	if err := r.Get(ctx, client.ObjectKeyFromObject(server), &sts); err == nil && ptr.Deref(sts.Spec.Replicas, 1) != 0 {
		// The server controller has not scaled down yet.
		return ctrl.Result{RequeueAfter: pollInterval}, nil
	}

	job := uploadJob(snap, server, r.DatactlImage, r.uploadTimeout())
	if err := controllerutil.SetControllerReference(snap, job, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, err
	}
	snap.Status.JobName = job.Name
	snap.Status.Phase = vtv1.SnapshotUploading
	snap.Status.Message = "uploading"
	return ctrl.Result{RequeueAfter: pollInterval}, nil
}

func (r *VTTestServerSnapshotReconciler) upload(ctx context.Context, snap *vtv1.VTTestServerSnapshot, server *vtv1.VTTestServer) (ctrl.Result, error) {
	// The Job has its own deadline. This check also covers a Job that is gone or never started.
	if started := snap.Status.PhaseStartedAt; started != nil && time.Since(started.Time) > r.uploadTimeout()+time.Minute {
		return r.failSnapshot(ctx, snap, server, fmt.Sprintf("the upload did not finish within %s", r.uploadTimeout()))
	}
	var job batchv1.Job
	if err := r.Get(ctx, client.ObjectKey{Namespace: snap.Namespace, Name: snap.Status.JobName}, &job); err != nil {
		if apierrors.IsNotFound(err) {
			snap.Status.Phase = vtv1.SnapshotQuiescing
			return ctrl.Result{RequeueAfter: pollInterval}, nil
		}
		return ctrl.Result{}, err
	}
	done, failed := jobDone(&job)
	switch {
	case failed:
		msg, _ := r.jobMessage(ctx, &job)
		if msg.Error == "" {
			msg.Error = "the snapshot Job failed" + jobFailureReason(&job) + ": see the Job pod logs"
		}
		return r.failSnapshot(ctx, snap, server, msg.Error)
	case !done:
		return ctrl.Result{RequeueAfter: pollInterval}, nil
	}
	msg, err := r.jobMessage(ctx, &job)
	if err != nil {
		return ctrl.Result{}, err
	}
	now := metav1.Now()
	snap.Status.CompletedAt = &now
	snap.Status.SHA256 = msg.SHA256
	snap.Status.SizeBytes = msg.SizeBytes
	snap.Status.S3URI = s3URI(snap)
	snap.Status.Fingerprint = &vtv1.SnapshotFingerprint{Keyspaces: server.Spec.Keyspaces}
	snap.Status.Message = ""
	snap.Status.Phase = vtv1.SnapshotReady
	meta.SetStatusCondition(&snap.Status.Conditions, metav1.Condition{
		Type: vtv1.ConditionSnapshot, Status: metav1.ConditionTrue, Reason: "Uploaded", ObservedGeneration: snap.Generation,
	})
	observeSnapshotDone(snap, now.Time)
	r.Recorder.Eventf(snap, nil, corev1.EventTypeNormal, "Uploaded", "Upload", "snapshot %s uploaded to %s", snap.Name, snap.Status.S3URI)
	if err := r.release(ctx, snap, server); err != nil {
		return ctrl.Result{}, err
	}
	res, err := r.checkExpiry(ctx, snap)
	// Come back once the Ready status is stored: then the Ready case removes the Job. Nothing else
	// triggers another pass for a finished snapshot without a TTL.
	if err == nil && (res.RequeueAfter == 0 || res.RequeueAfter > 2*time.Second) {
		res.RequeueAfter = 2 * time.Second
	}
	return res, err
}

func (r *VTTestServerSnapshotReconciler) failSnapshot(ctx context.Context, snap *vtv1.VTTestServerSnapshot, server *vtv1.VTTestServer, msg string) (ctrl.Result, error) {
	snap.Status.Phase = vtv1.SnapshotFailed
	snap.Status.Message = msg
	meta.SetStatusCondition(&snap.Status.Conditions, metav1.Condition{
		Type: vtv1.ConditionSnapshot, Status: metav1.ConditionFalse, Reason: "Failed", Message: msg, ObservedGeneration: snap.Generation,
	})
	snapshotFailures.Inc()
	r.Recorder.Eventf(snap, nil, corev1.EventTypeWarning, "Failed", "Upload", "%s", msg)
	// Stop an upload that is still running, so that it does not keep the volume.
	if err := r.deleteUploadJob(ctx, snap); err != nil {
		return ctrl.Result{}, err
	}
	if server != nil {
		if err := r.release(ctx, snap, server); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *VTTestServerSnapshotReconciler) deleteUploadJob(ctx context.Context, snap *vtv1.VTTestServerSnapshot) error {
	if snap.Status.JobName == "" {
		return nil
	}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: snap.Status.JobName, Namespace: snap.Namespace}}
	return client.IgnoreNotFound(r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)))
}

// release removes the hold, if this snapshot owns it, so the server starts again.
func (r *VTTestServerSnapshotReconciler) release(ctx context.Context, snap *vtv1.VTTestServerSnapshot, server *vtv1.VTTestServer) error {
	if server.Annotations[vtv1.SnapshotHoldAnnot] != snap.Name {
		return nil
	}
	patch := client.MergeFrom(server.DeepCopy())
	delete(server.Annotations, vtv1.SnapshotHoldAnnot)
	return client.IgnoreNotFound(r.Patch(ctx, server, patch))
}

func (r *VTTestServerSnapshotReconciler) checkExpiry(ctx context.Context, snap *vtv1.VTTestServerSnapshot) (ctrl.Result, error) {
	if snap.Spec.TTL == nil || snap.Status.CompletedAt == nil {
		return ctrl.Result{}, nil
	}
	left := time.Until(snap.Status.CompletedAt.Add(snap.Spec.TTL.Duration))
	if left > 0 {
		return ctrl.Result{RequeueAfter: left}, nil
	}
	snap.Status.Phase = vtv1.SnapshotDeleting
	return r.expire(ctx, snap)
}

// expire deletes the S3 objects of an expired snapshot with a Job.
func (r *VTTestServerSnapshotReconciler) expire(ctx context.Context, snap *vtv1.VTTestServerSnapshot) (ctrl.Result, error) {
	done, err := r.runDelete(ctx, snap)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !done {
		return ctrl.Result{RequeueAfter: pollInterval}, nil
	}
	snap.Status.Phase = vtv1.SnapshotExpired
	snap.Status.Message = "the S3 objects were deleted after the TTL"
	return ctrl.Result{}, nil
}

// finalize runs when the object is deleted. With deletionPolicy Delete, it removes the S3 objects first.
func (r *VTTestServerSnapshotReconciler) finalize(ctx context.Context, snap *vtv1.VTTestServerSnapshot) (ctrl.Result, error) {
	var server vtv1.VTTestServer
	if err := r.Get(ctx, client.ObjectKey{Namespace: snap.Namespace, Name: snap.Spec.ServerRef.Name}, &server); err == nil {
		if err := r.release(ctx, snap, &server); err != nil {
			return ctrl.Result{}, err
		}
	}
	if !controllerutil.ContainsFinalizer(snap, vtv1.SnapshotFinalizer) {
		return ctrl.Result{}, nil
	}
	// A snapshot that never reached the store has nothing to delete, but the delete is harmless.
	done, err := r.runDelete(ctx, snap)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !done {
		return ctrl.Result{RequeueAfter: pollInterval}, nil
	}
	controllerutil.RemoveFinalizer(snap, vtv1.SnapshotFinalizer)
	return ctrl.Result{}, r.Update(ctx, snap)
}

// runDelete creates the delete Job and reports whether it finished. A failed Job is retried by Kubernetes up to its backoff limit.
func (r *VTTestServerSnapshotReconciler) runDelete(ctx context.Context, snap *vtv1.VTTestServerSnapshot) (bool, error) {
	job := deleteJob(snap, r.DatactlImage)
	if err := controllerutil.SetControllerReference(snap, job, r.Scheme); err != nil {
		return false, err
	}
	var existing batchv1.Job
	err := r.Get(ctx, client.ObjectKeyFromObject(job), &existing)
	if apierrors.IsNotFound(err) {
		return false, r.Create(ctx, job)
	}
	if err != nil {
		return false, err
	}
	done, failed := jobDone(&existing)
	if failed {
		msg, _ := r.jobMessage(ctx, &existing)
		snap.Status.Message = "delete Job failed: " + msg.Error
		return false, nil
	}
	return done, nil
}

// jobFailureReason says why a Job failed, for example DeadlineExceeded.
func jobFailureReason(j *batchv1.Job) string {
	for _, c := range j.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue && c.Reason != "" {
			return " (" + c.Reason + ")"
		}
	}
	return ""
}

func jobDone(j *batchv1.Job) (done, failed bool) {
	for _, c := range j.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			return true, false
		case batchv1.JobFailed:
			return false, true
		}
	}
	return false, false
}

// jobMessage reads the termination message that datactl wrote in the Job pod.
func (r *VTTestServerSnapshotReconciler) jobMessage(ctx context.Context, job *batchv1.Job) (manifest.TerminationMessage, error) {
	var msg manifest.TerminationMessage
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(job.Namespace), client.MatchingLabels{"job-name": job.Name}); err != nil {
		return msg, err
	}
	for _, p := range pods.Items {
		for _, c := range p.Status.ContainerStatuses {
			if t := c.State.Terminated; t != nil && t.Message != "" {
				if err := json.Unmarshal([]byte(t.Message), &msg); err == nil {
					return msg, nil
				}
				msg.Error = t.Message
				return msg, nil
			}
		}
	}
	return msg, nil
}

func s3URI(snap *vtv1.VTTestServerSnapshot) string {
	p := snap.Spec.S3.Prefix
	if p != "" {
		p += "/"
	}
	return fmt.Sprintf("s3://%s/%ssnapshots/%s", snap.Spec.S3.Bucket, p, snap.Name)
}

func jobPodSpec(snap *vtv1.VTTestServerSnapshot, image string, args []string, volumes []corev1.Volume, mounts []corev1.VolumeMount) corev1.PodSpec {
	return corev1.PodSpec{
		RestartPolicy:      corev1.RestartPolicyNever,
		ServiceAccountName: snap.Spec.ServiceAccountName,
		SecurityContext:    podSecurityContext(),
		Volumes:            volumes,
		Containers: []corev1.Container{{
			Name:                     "datactl",
			Image:                    image,
			Command:                  []string{datactlBinary},
			Args:                     args,
			Resources:                datactlResources(),
			EnvFrom:                  credentialsEnvFrom(snap.Spec.S3),
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
			SecurityContext:          containerSecurityContext(),
			VolumeMounts:             mounts,
		}},
	}
}

func uploadJob(snap *vtv1.VTTestServerSnapshot, server *vtv1.VTTestServer, image string, deadline time.Duration) *batchv1.Job {
	args := append([]string{"snapshot", argDataDir, dataDir, "--name", snap.Name}, s3Args(snap.Spec.S3)...)
	for _, t := range snap.Spec.Tags {
		args = append(args, "--tag", t)
	}
	args = append(args, fingerprintArgs(server)...)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: snap.Name + "-upload", Namespace: snap.Namespace, Labels: map[string]string{jobLabel: snap.Name}},
		Spec: batchv1.JobSpec{
			BackoffLimit:          ptr.To[int32](0),
			ActiveDeadlineSeconds: new(int64(deadline.Seconds())),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{jobLabel: snap.Name}},
				Spec: jobPodSpec(snap, image, args,
					[]corev1.Volume{{Name: dataVolume, VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName(server)},
					}}},
					[]corev1.VolumeMount{{Name: dataVolume, MountPath: dataDir}}),
			},
		},
	}
}

func deleteJob(snap *vtv1.VTTestServerSnapshot, image string) *batchv1.Job {
	args := append([]string{"delete", "--name", snap.Name}, s3Args(snap.Spec.S3)...)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: snap.Name + "-delete", Namespace: snap.Namespace, Labels: map[string]string{jobLabel: snap.Name}},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To[int32](3),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{jobLabel: snap.Name}},
				Spec:       jobPodSpec(snap, image, args, nil, nil),
			},
		},
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *VTTestServerSnapshotReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorder("vttestserver-operator")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&vtv1.VTTestServerSnapshot{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&batchv1.Job{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}).
		Named("vttestserversnapshot").
		Complete(r)
}
