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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	vtv1 "github.com/likesavabutworse/vttestserver-operator/api/v1alpha1"
	"github.com/likesavabutworse/vttestserver-operator/internal/snapshot/manifest"
)

func snapReconciler() *VTTestServerSnapshotReconciler {
	return &VTTestServerSnapshotReconciler{
		Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: events.NewFakeRecorder(100), DatactlImage: "datactl:test",
	}
}

func reconcileSnap(r *VTTestServerSnapshotReconciler, s *vtv1.VTTestServerSnapshot) ctrl.Result {
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(s)})
	Expect(err).NotTo(HaveOccurred())
	return res
}

func getSnap(s *vtv1.VTTestServerSnapshot) *vtv1.VTTestServerSnapshot {
	out := &vtv1.VTTestServerSnapshot{}
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(s), out)).To(Succeed())
	return out
}

func newSnap(ns, name, server string) *vtv1.VTTestServerSnapshot {
	return &vtv1.VTTestServerSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: vtv1.VTTestServerSnapshotSpec{
			ServerRef:          vtv1.ServerRef{Name: server},
			ServiceAccountName: "snapshots",
			S3:                 vtv1.S3Spec{Bucket: "bkt", Prefix: "pre", Endpoint: "http://silo:9000", ForcePathStyle: true},
			Tags:               []string{"latest"},
		},
	}
}

// persistentReadyServer makes a persistent server and marks it Ready, as its controller would.
func persistentReadyServer(ns, name string) *vtv1.VTTestServer {
	s := baseServer(ns, name)
	s.Spec.Keyspaces = []vtv1.KeyspaceSpec{{Name: "commerce", Shards: 1}, {Name: "customer", Shards: 1}}
	s.Spec.Storage = vtv1.StorageSpec{Type: vtv1.StoragePersistent}
	Expect(k8sClient.Create(ctx, s)).To(Succeed())
	s.Status.Phase = vtv1.PhaseReady
	Expect(k8sClient.Status().Update(ctx, s)).To(Succeed())
	return s
}

// finishJob marks a Job as finished and creates its pod with a termination message.
func finishJob(job *batchv1.Job, succeeded bool, msg manifest.TerminationMessage) {
	raw, _ := json.Marshal(msg)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-pod", Namespace: job.Namespace, Labels: map[string]string{"job-name": job.Name}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "datactl", Image: "x"}}},
	}
	Expect(k8sClient.Create(ctx, pod)).To(Succeed())
	code := int32(0)
	if !succeeded {
		code = 1
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "datactl",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Message: string(raw)}}}}
	Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(job), job)).To(Succeed())
	now := metav1.Now()
	job.Status.StartTime = &now
	if succeeded {
		job.Status.CompletionTime = &now
		job.Status.Succeeded = 1
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue},
			{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
		}
	} else {
		job.Status.Failed = 1
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue},
			{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
		}
	}
	Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
}

var _ = Describe("VTTestServerSnapshot controller", func() {
	var (
		ns string
		r  *VTTestServerSnapshotReconciler
	)
	BeforeEach(func() {
		ns = newNamespace()
		r = snapReconciler()
	})

	It("waits for a server that does not exist", func() {
		snap := newSnap(ns, "snap-1", "missing")
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		res := reconcileSnap(r, snap)
		Expect(res.RequeueAfter).NotTo(BeZero())
		got := getSnap(snap)
		Expect(got.Status.Phase).To(Equal(vtv1.SnapshotPending))
		Expect(got.Status.Message).To(ContainSubstring("does not exist"))
	})

	It("fails for a server with an ephemeral volume", func() {
		s := baseServer(ns, "shop")
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		snap := newSnap(ns, "snap-1", "shop")
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		reconcileSnap(r, snap)
		got := getSnap(snap)
		Expect(got.Status.Phase).To(Equal(vtv1.SnapshotFailed))
		Expect(got.Status.Message).To(ContainSubstring("persistent storage"))
	})

	It("waits while the server is not Ready or held by another snapshot", func() {
		s := baseServer(ns, "shop")
		s.Spec.Storage = vtv1.StorageSpec{Type: vtv1.StoragePersistent}
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		snap := newSnap(ns, "snap-1", "shop")
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		reconcileSnap(r, snap)
		Expect(getSnap(snap).Status.Message).To(ContainSubstring("waiting for Ready"))

		s.Status.Phase = vtv1.PhaseReady
		Expect(k8sClient.Status().Update(ctx, s)).To(Succeed())
		s = getServer(s)
		s.Annotations = map[string]string{vtv1.SnapshotHoldAnnot: "other"}
		Expect(k8sClient.Update(ctx, s)).To(Succeed())
		reconcileSnap(r, snap)
		Expect(getSnap(snap).Status.Message).To(ContainSubstring(`held by snapshot "other"`))
	})

	It("lets only one of two snapshots take the hold of a server", func() {
		s := persistentReadyServer(ns, "shop")
		first, second := newSnap(ns, "first", "shop"), newSnap(ns, "second", "shop")
		Expect(k8sClient.Create(ctx, first)).To(Succeed())
		Expect(k8sClient.Create(ctx, second)).To(Succeed())

		// Both controllers workers read the server before either one writes the hold.
		stale := getServer(s)
		reconcileSnap(r, first)
		Expect(getServer(s).Annotations[vtv1.SnapshotHoldAnnot]).To(Equal("first"))

		_, err := r.start(ctx, getSnap(second), stale)
		Expect(apierrors.IsConflict(err)).To(BeTrue(), "the stale write must fail, got %v", err)
		Expect(getServer(s).Annotations[vtv1.SnapshotHoldAnnot]).To(Equal("first"))

		By("the loser sees the hold on its next pass")
		reconcileSnap(r, second)
		Expect(getSnap(second).Status.Message).To(ContainSubstring(`held by snapshot "first"`))
	})

	It("does not take the hold while the server restores", func() {
		s := baseServer(ns, "shop")
		s.Spec.Storage = vtv1.StorageSpec{Type: vtv1.StoragePersistent}
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		s.Status.Phase = vtv1.PhaseRestoring
		Expect(k8sClient.Status().Update(ctx, s)).To(Succeed())
		snap := newSnap(ns, "snap-1", "shop")
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())

		reconcileSnap(r, snap)
		got := getSnap(snap)
		Expect(got.Status.Phase).To(Equal(vtv1.SnapshotPending))
		Expect(got.Status.Message).To(ContainSubstring("Restoring"))
		Expect(getServer(s).Annotations).NotTo(HaveKey(vtv1.SnapshotHoldAnnot))
	})

	It("releases the server when the snapshot is deleted while it holds the server", func() {
		s := persistentReadyServer(ns, "shop")
		snap := newSnap(ns, "snap-1", "shop")
		// The finalizer keeps the object, so that its controller sees the deletion.
		snap.Spec.DeletionPolicy = vtv1.DeletionDelete
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		reconcileSnap(r, snap)
		Expect(getServer(s).Annotations[vtv1.SnapshotHoldAnnot]).To(Equal("snap-1"))

		Expect(k8sClient.Delete(ctx, getSnap(snap))).To(Succeed())
		reconcileSnap(r, snap)
		Expect(getServer(s).Annotations).NotTo(HaveKey(vtv1.SnapshotHoldAnnot))
	})

	It("stops the server, uploads with a Job, records the result and releases the server", func() {
		s := persistentReadyServer(ns, "shop")
		snap := newSnap(ns, "snap-1", "shop")
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())

		reconcileSnap(r, snap)
		Expect(getSnap(snap).Status.Phase).To(Equal(vtv1.SnapshotQuiescing))
		Expect(getServer(s).Annotations[vtv1.SnapshotHoldAnnot]).To(Equal("snap-1"))

		By("the pod still runs: no Job yet")
		pod := makePod(s, true, nil)
		reconcileSnap(r, snap)
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "snap-1-upload"}, &batchv1.Job{})).NotTo(Succeed())

		By("the pod is gone: the Job starts")
		Expect(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0))).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{}))
		}).Should(BeTrue())
		reconcileSnap(r, snap)
		var job batchv1.Job
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "snap-1-upload"}, &job)).To(Succeed())
		Expect(getSnap(snap).Status.Phase).To(Equal(vtv1.SnapshotUploading))
		c := job.Spec.Template.Spec.Containers[0]
		Expect(c.Args).To(ContainElements("snapshot", "--name", "snap-1", "--bucket", "bkt", "--prefix", "pre",
			"--endpoint", "http://silo:9000", "--force-path-style", "--tag", "latest",
			"--keyspace", "commerce:1", "--keyspace", "customer:1"))
		Expect(job.Spec.Template.Spec.ServiceAccountName).To(Equal("snapshots"))
		Expect(c.Resources.Requests.Memory().String()).To(Equal("128Mi"))
		Expect(c.Resources.Limits.Memory().String()).To(Equal("256Mi"))
		Expect(job.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName).To(Equal("data-shop-0"))
		Expect(job.OwnerReferences).To(HaveLen(1))

		By("the Job finishes")
		finishJob(&job, true, manifest.TerminationMessage{Name: "snap-1", SHA256: "abc123", SizeBytes: 4096})
		reconcileSnap(r, snap)
		got := getSnap(snap)
		Expect(got.Status.Phase).To(Equal(vtv1.SnapshotReady))
		Expect(got.Status.SHA256).To(Equal("abc123"))
		Expect(got.Status.SizeBytes).To(Equal(int64(4096)))
		Expect(got.Status.S3URI).To(Equal("s3://bkt/pre/snapshots/snap-1"))
		Expect(got.Status.CompletedAt).NotTo(BeNil())
		Expect(got.Status.Fingerprint.Keyspaces).To(HaveLen(2))
		Expect(getServer(s).Annotations).NotTo(HaveKey(vtv1.SnapshotHoldAnnot))

		By("the next pass removes the finished Job: its pod would keep the volume claim in use")
		reconcileSnap(r, snap)
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(&job), &batchv1.Job{}))
		}).Should(BeTrue())
		Expect(getSnap(snap).Status.Phase).To(Equal(vtv1.SnapshotReady))
	})

	It("does not overwrite a newer status with the result of a pass that started from a stale copy", func() {
		persistentReadyServer(ns, "shop")
		snap := newSnap(ns, "snap-1", "shop")
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		snap.Status.Phase = vtv1.SnapshotUploading
		snap.Status.JobName = "snap-1-upload"
		now := metav1.Now()
		snap.Status.PhaseStartedAt = &now
		Expect(k8sClient.Status().Update(ctx, snap)).To(Succeed())
		stale := getSnap(snap)

		By("another pass ends the snapshot and deletes the Job")
		snap = getSnap(snap)
		snap.Status.Phase = vtv1.SnapshotFailed
		Expect(k8sClient.Status().Update(ctx, snap)).To(Succeed())

		By("a pass that still sees the old copy finds no Job and would go back to Quiescing")
		withWatch, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		r.Client = interceptor.NewClient(withWatch, interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if out, ok := obj.(*vtv1.VTTestServerSnapshot); ok {
					stale.DeepCopyInto(out)
					return nil
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(snap)})
		Expect(err).NotTo(HaveOccurred())
		Expect(getSnap(snap).Status.Phase).To(Equal(vtv1.SnapshotFailed))
	})

	It("fails with the Job error and releases the server", func() {
		s := persistentReadyServer(ns, "shop")
		snap := newSnap(ns, "snap-1", "shop")
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		reconcileSnap(r, snap)
		reconcileSnap(r, snap)
		var job batchv1.Job
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "snap-1-upload"}, &job)).To(Succeed())

		finishJob(&job, false, manifest.TerminationMessage{Error: "snapshot \"snap-1\" already exists"})
		reconcileSnap(r, snap)
		got := getSnap(snap)
		Expect(got.Status.Phase).To(Equal(vtv1.SnapshotFailed))
		Expect(got.Status.Message).To(ContainSubstring("already exists"))
		Expect(getServer(s).Annotations).NotTo(HaveKey(vtv1.SnapshotHoldAnnot))
	})

	holdBy := func(s *vtv1.VTTestServer, snapName string) {
		s = getServer(s)
		s.Annotations = map[string]string{vtv1.SnapshotHoldAnnot: snapName}
		Expect(k8sClient.Update(ctx, s)).To(Succeed())
	}
	inPhase := func(snap *vtv1.VTTestServerSnapshot, phase string, started time.Duration) {
		t := metav1.NewTime(time.Now().Add(-started))
		snap.Status.Phase = phase
		snap.Status.PhaseStartedAt = &t
		Expect(k8sClient.Status().Update(ctx, snap)).To(Succeed())
	}

	It("fails and releases the server when it does not stop in time", func() {
		s := persistentReadyServer(ns, "shop")
		snap := newSnap(ns, "slow", "shop")
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		holdBy(s, "slow")
		makePod(s, true, nil) // the pod never goes away
		inPhase(snap, vtv1.SnapshotQuiescing, 11*time.Minute)

		reconcileSnap(r, snap)
		got := getSnap(snap)
		Expect(got.Status.Phase).To(Equal(vtv1.SnapshotFailed))
		Expect(got.Status.Message).To(ContainSubstring("did not stop within 10m0s"))
		Expect(getServer(s).Annotations).NotTo(HaveKey(vtv1.SnapshotHoldAnnot))
	})

	It("keeps waiting before the quiesce deadline", func() {
		s := persistentReadyServer(ns, "shop")
		snap := newSnap(ns, "ok", "shop")
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		holdBy(s, "ok")
		makePod(s, true, nil)
		inPhase(snap, vtv1.SnapshotQuiescing, 2*time.Minute)
		reconcileSnap(r, snap)
		Expect(getSnap(snap).Status.Phase).To(Equal(vtv1.SnapshotQuiescing))
		Expect(getServer(s).Annotations).To(HaveKey(vtv1.SnapshotHoldAnnot))
	})

	It("fails, stops the Job and releases the server when the upload takes too long", func() {
		s := persistentReadyServer(ns, "shop")
		snap := newSnap(ns, "stuck", "shop")
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		holdBy(s, "stuck")
		reconcileSnap(r, snap) // takes the hold, Quiescing
		reconcileSnap(r, snap) // no pod: creates the Job
		var job batchv1.Job
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "stuck-upload"}, &job)).To(Succeed())
		Expect(*job.Spec.ActiveDeadlineSeconds).To(Equal(int64(30 * 60)))

		inPhase(getSnap(snap), vtv1.SnapshotUploading, 32*time.Minute)
		reconcileSnap(r, snap)
		got := getSnap(snap)
		Expect(got.Status.Phase).To(Equal(vtv1.SnapshotFailed))
		Expect(got.Status.Message).To(ContainSubstring("did not finish within 30m0s"))
		Expect(getServer(s).Annotations).NotTo(HaveKey(vtv1.SnapshotHoldAnnot))
		Eventually(func() bool {
			err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "stuck-upload"}, &batchv1.Job{})
			if apierrors.IsNotFound(err) {
				return true
			}
			// envtest has no garbage collector: a Job with a deletion timestamp counts as stopped.
			var j batchv1.Job
			return err == nil && k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "stuck-upload"}, &j) == nil && !j.DeletionTimestamp.IsZero()
		}).Should(BeTrue())
	})

	It("names the reason of a failed Job, for example DeadlineExceeded", func() {
		s := persistentReadyServer(ns, "shop")
		snap := newSnap(ns, "late", "shop")
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		holdBy(s, "late")
		reconcileSnap(r, snap)
		reconcileSnap(r, snap)
		var job batchv1.Job
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "late-upload"}, &job)).To(Succeed())
		now := metav1.Now()
		job.Status.StartTime = &now
		job.Status.Failed = 1
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: "DeadlineExceeded"},
			{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "DeadlineExceeded"},
		}
		Expect(k8sClient.Status().Update(ctx, &job)).To(Succeed())
		reconcileSnap(r, snap)
		Expect(getSnap(snap).Status.Message).To(ContainSubstring("(DeadlineExceeded)"))
	})

	It("fails when the server is deleted during the snapshot", func() {
		s := persistentReadyServer(ns, "shop")
		snap := newSnap(ns, "orphan", "shop")
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		reconcileSnap(r, snap)
		Expect(getSnap(snap).Status.Phase).To(Equal(vtv1.SnapshotQuiescing))
		Expect(k8sClient.Delete(ctx, s)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(s), &vtv1.VTTestServer{}))
		}).Should(BeTrue())
		reconcileSnap(r, snap)
		got := getSnap(snap)
		Expect(got.Status.Phase).To(Equal(vtv1.SnapshotFailed))
		Expect(got.Status.Message).To(ContainSubstring("was deleted during the snapshot"))
	})

	It("deletes the S3 objects with a Job after the TTL and ends as Expired", func() {
		snap := newSnap(ns, "old", "shop")
		snap.Spec.TTL = &metav1.Duration{Duration: time.Minute}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		done := metav1.NewTime(time.Now().Add(-time.Hour))
		snap.Status.Phase = vtv1.SnapshotReady
		snap.Status.CompletedAt = &done
		Expect(k8sClient.Status().Update(ctx, snap)).To(Succeed())

		reconcileSnap(r, snap)
		Expect(getSnap(snap).Status.Phase).To(Equal(vtv1.SnapshotDeleting))
		var job batchv1.Job
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "old-delete"}, &job)).To(Succeed())
		Expect(job.Spec.Template.Spec.Containers[0].Args).To(ContainElements("delete", "--name", "old"))

		finishJob(&job, true, manifest.TerminationMessage{})
		reconcileSnap(r, snap)
		Expect(getSnap(snap).Status.Phase).To(Equal(vtv1.SnapshotExpired))
	})

	It("waits for the TTL of a fresh snapshot", func() {
		snap := newSnap(ns, "fresh", "shop")
		snap.Spec.TTL = &metav1.Duration{Duration: time.Hour}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		now := metav1.Now()
		snap.Status.Phase = vtv1.SnapshotReady
		snap.Status.CompletedAt = &now
		Expect(k8sClient.Status().Update(ctx, snap)).To(Succeed())
		res := reconcileSnap(r, snap)
		Expect(res.RequeueAfter).To(BeNumerically(">", 59*time.Minute))
		Expect(getSnap(snap).Status.Phase).To(Equal(vtv1.SnapshotReady))
	})

	It("adds no finalizer for Retain, and deletes the S3 objects first for Delete", func() {
		keep := newSnap(ns, "keep", "shop")
		Expect(k8sClient.Create(ctx, keep)).To(Succeed())
		reconcileSnap(r, keep)
		Expect(getSnap(keep).Finalizers).To(BeEmpty())

		del := newSnap(ns, "del", "shop")
		del.Spec.DeletionPolicy = vtv1.DeletionDelete
		Expect(k8sClient.Create(ctx, del)).To(Succeed())
		reconcileSnap(r, del)
		Expect(getSnap(del).Finalizers).To(ContainElement(vtv1.SnapshotFinalizer))

		Expect(k8sClient.Delete(ctx, getSnap(del))).To(Succeed())
		reconcileSnap(r, del)
		var job batchv1.Job
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "del-delete"}, &job)).To(Succeed())
		Expect(job.Spec.Template.Spec.Containers[0].Args).To(ContainElements("delete", "--name", "del", "--bucket", "bkt"))
		Expect(getSnap(del).Finalizers).NotTo(BeEmpty())

		finishJob(&job, true, manifest.TerminationMessage{})
		reconcileSnap(r, del)
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(del), &vtv1.VTTestServerSnapshot{}))
		}).Should(BeTrue())
	})
})
