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
	"strings"
	"sync"
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	vtv1 "github.com/likesavabutworse/vttestserver-operator/api/v1alpha1"
	"github.com/likesavabutworse/vttestserver-operator/internal/snapshot/manifest"
)

const (
	commerceVSchema = `{}`
	customerVSchema = `{"sharded": true, "vindexes": {"xxhash": {"type": "xxhash"}},
		"tables": {"customer": {"column_vindexes": [{"column": "customer_id", "name": "xxhash"}]}}}`
)

// fakeVitess records the calls of the controller.
type fakeVitess struct {
	mu       sync.Mutex
	vschemas []string
	sql      []string
	dials    atomic.Int32
	failWith error
}

func (f *fakeVitess) dial(context.Context, string, string) (VitessClient, error) {
	f.dials.Add(1)
	return f, nil
}

func (f *fakeVitess) ApplyVSchema(_ context.Context, ks string, raw []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return f.failWith
	}
	f.vschemas = append(f.vschemas, ks)
	return nil
}

func (f *fakeVitess) ExecSQL(_ context.Context, ks string, stmts []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sql = append(f.sql, ks+": "+strings.Join(stmts, " | "))
	return nil
}

func (f *fakeVitess) Close() error { return nil }

func (f *fakeVitess) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.vschemas) + len(f.sql)
}

var nsCounter atomic.Int32

func newNamespace() string {
	name := fmt.Sprintf("test-%d", nsCounter.Add(1))
	Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})).To(Succeed())
	return name
}

func baseServer(ns, name string) *vtv1.VTTestServer {
	return &vtv1.VTTestServer{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: vtv1.VTTestServerSpec{
			Image:     "vitess/vttestserver:mysql84",
			Keyspaces: []vtv1.KeyspaceSpec{{Name: "commerce", Shards: 1}},
		},
	}
}

func configMap(ns, name string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Data: data}
}

func serverReconciler(f *fakeVitess) *VTTestServerReconciler {
	return &VTTestServerReconciler{
		Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: events.NewFakeRecorder(100),
		DatactlImage: "datactl:test", Dial: f.dial,
	}
}

func reconcileServer(r *VTTestServerReconciler, s *vtv1.VTTestServer) ctrl.Result {
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(s)})
	Expect(err).NotTo(HaveOccurred())
	return res
}

func getServer(s *vtv1.VTTestServer) *vtv1.VTTestServer {
	out := &vtv1.VTTestServer{}
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(s), out)).To(Succeed())
	return out
}

// makePod creates the pod that a StatefulSet would make, because envtest has no controller manager or kubelet.
func makePod(s *vtv1.VTTestServer, ready bool, mutate func(*corev1.Pod)) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName(s), Namespace: s.Namespace, Labels: labelsFor(s)},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: serverContainer, Image: "x"}}},
	}
	Expect(k8sClient.Create(ctx, pod)).To(Succeed())
	setPodStatus(pod, ready, mutate)
	return pod
}

func setPodStatus(pod *corev1.Pod, ready bool, mutate func(*corev1.Pod)) {
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), pod)).To(Succeed())
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}
	if mutate != nil {
		mutate(pod)
	}
	Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
}

var _ = Describe("VTTestServer API validation", func() {
	var ns string
	BeforeEach(func() { ns = newNamespace() })

	It("rejects several shards without a VSchema or a restore", func() {
		s := baseServer(ns, "a")
		s.Spec.Keyspaces = []vtv1.KeyspaceSpec{{Name: "customer", Shards: 2}}
		err := k8sClient.Create(ctx, s)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("needs spec.vschema or spec.restoreFrom"))
	})

	It("accepts several shards with a VSchema reference", func() {
		s := baseServer(ns, "a")
		s.Spec.Keyspaces = []vtv1.KeyspaceSpec{{Name: "customer", Shards: 2}}
		s.Spec.VSchema = &vtv1.VSchemaSource{ConfigMapRef: corev1.LocalObjectReference{Name: "vs"}}
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
	})

	It("keeps keyspaces immutable", func() {
		s := baseServer(ns, "a")
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		s.Spec.Keyspaces[0].Shards = 1
		s.Spec.Keyspaces = append(s.Spec.Keyspaces, vtv1.KeyspaceSpec{Name: "extra", Shards: 1})
		err := k8sClient.Update(ctx, s)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("keyspaces is immutable"))
	})

	It("keeps restoreFrom immutable, also when it was not set at creation", func() {
		s := baseServer(ns, "a")
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		s.Spec.RestoreFrom = &vtv1.RestoreSpec{Tag: "latest", S3: vtv1.S3Spec{Bucket: "b"}}
		err := k8sClient.Update(ctx, s)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("restoreFrom is immutable"))

		r := baseServer(ns, "b")
		r.Spec.RestoreFrom = &vtv1.RestoreSpec{Tag: "latest", S3: vtv1.S3Spec{Bucket: "b"}}
		Expect(k8sClient.Create(ctx, r)).To(Succeed())
		r.Spec.RestoreFrom.Tag = "other"
		Expect(k8sClient.Update(ctx, r)).NotTo(Succeed())
	})

	It("needs exactly one of name and tag in restoreFrom", func() {
		for name, rs := range map[string]*vtv1.RestoreSpec{
			"both":    {Name: "n", Tag: "t", S3: vtv1.S3Spec{Bucket: "b"}},
			"neither": {S3: vtv1.S3Spec{Bucket: "b"}},
		} {
			s := baseServer(ns, "r-"+name)
			s.Spec.RestoreFrom = rs
			err := k8sClient.Create(ctx, s)
			Expect(err).To(HaveOccurred(), name)
			Expect(err.Error()).To(ContainSubstring("exactly one of name and tag"))
		}
	})

	It("rejects initSQL for an unknown keyspace", func() {
		s := baseServer(ns, "a")
		s.Spec.InitSQL = []vtv1.InitSQLSource{{Keyspace: "nope", ConfigMapKeyRef: vtv1.ConfigMapKeyRef{Name: "c", Key: "k"}}}
		err := k8sClient.Create(ctx, s)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("initSQL keyspace must be one of spec.keyspaces"))
	})

	It("rejects an invalid keyspace name and zero shards", func() {
		s := baseServer(ns, "a")
		s.Spec.Keyspaces = []vtv1.KeyspaceSpec{{Name: "bad-name", Shards: 1}}
		Expect(k8sClient.Create(ctx, s)).NotTo(Succeed())
		s.Spec.Keyspaces = []vtv1.KeyspaceSpec{{Name: "ok", Shards: 0}}
		Expect(k8sClient.Create(ctx, s)).NotTo(Succeed())
	})

	It("keeps a snapshot spec immutable", func() {
		snap := &vtv1.VTTestServerSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: ns},
			Spec: vtv1.VTTestServerSnapshotSpec{
				ServerRef: vtv1.ServerRef{Name: "a"}, S3: vtv1.S3Spec{Bucket: "b"}, Tags: []string{"latest"},
			},
		}
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		Expect(snap.Spec.Consistency).To(Equal("clean"))
		Expect(snap.Spec.DeletionPolicy).To(Equal("Retain"))
		snap.Spec.Tags = []string{"other"}
		Expect(k8sClient.Update(ctx, snap)).NotTo(Succeed())
	})
})

var _ = Describe("VTTestServer controller", func() {
	var (
		ns string
		f  *fakeVitess
		r  *VTTestServerReconciler
	)
	BeforeEach(func() {
		ns = newNamespace()
		f = &fakeVitess{}
		r = serverReconciler(f)
	})

	It("creates the Services, the StatefulSet and the status endpoints", func() {
		s := baseServer(ns, "shop")
		s.Spec.MySQLConfig = map[string]string{"innodb_buffer_pool_size": "128M"}
		s.Spec.ExtraArgs = []string{"--no-scatter"}
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		reconcileServer(r, s)

		var sts appsv1.StatefulSet
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "shop"}, &sts)).To(Succeed())
		Expect(*sts.Spec.Replicas).To(Equal(int32(1)))
		c := sts.Spec.Template.Spec.Containers[0]
		Expect(c.Resources.Requests.Memory().String()).To(Equal("768Mi"))
		Expect(c.Resources.Limits.Memory().String()).To(Equal("1536Mi"))
		Expect(c.Resources.Requests.StorageEphemeral().String()).To(Equal("2Gi"))
		Expect(c.Command).To(Equal([]string{"/vt/bin/vttestserver"}))
		Expect(c.Args).To(ContainElements("--keyspaces", "commerce", "--num-shards", "1", "--persistent-mode", "--data-dir=/vt/vtdataroot"))
		Expect(c.Args[len(c.Args)-1]).To(Equal("--no-scatter"))
		Expect(c.Args).NotTo(ContainElement("--vschema-ddl-authorized-users=%"))
		Expect(c.Args).To(ContainElement("--extra-my-cnf=/etc/vttest/mysql/extra.cnf"))
		Expect(c.Env).To(BeEmpty())
		myCnf := sts.Spec.Template.Spec.InitContainers
		Expect(myCnf).To(HaveLen(1))
		Expect(myCnf[0].Name).To(Equal("apply-my-cnf"))
		Expect(myCnf[0].Resources.Limits.Memory().String()).To(Equal("256Mi"))
		Expect(myCnf[0].Args).To(Equal([]string{"apply-my-cnf", "--data-dir", "/vt/vtdataroot", "--file", "/etc/vttest/mysql/extra.cnf"}))
		Expect(sts.OwnerReferences).To(HaveLen(1))

		var mysqlSvc, grpcSvc corev1.Service
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "shop"}, &mysqlSvc)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "shop-grpc"}, &grpcSvc)).To(Succeed())
		Expect(mysqlSvc.Spec.Ports[0].Port).To(Equal(int32(15003)))
		Expect(grpcSvc.Spec.Ports[0].Port).To(Equal(int32(15001)))

		var cnf corev1.ConfigMap
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "shop-mycnf"}, &cnf)).To(Succeed())
		Expect(cnf.Data["extra.cnf"]).To(ContainSubstring("innodb_buffer_pool_size = 128M"))

		got := getServer(s)
		Expect(got.Status.Phase).To(Equal(vtv1.PhasePending))
		Expect(got.Status.Endpoints.MySQL).To(Equal("shop." + ns + ".svc:15003"))
		Expect(got.Status.Endpoints.GRPC).To(Equal("shop-grpc." + ns + ".svc:15001"))
	})

	It("rolls the pod when mysqlConfig changes", func() {
		s := baseServer(ns, "shop")
		s.Spec.MySQLConfig = map[string]string{"max_connections": "600"}
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		reconcileServer(r, s)
		hash := func() string {
			var sts appsv1.StatefulSet
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "shop"}, &sts)).To(Succeed())
			return sts.Spec.Template.Annotations["vttestserver-operator.io/mysql-config-hash"]
		}
		first := hash()
		Expect(first).NotTo(BeEmpty())

		reconcileServer(r, s)
		Expect(hash()).To(Equal(first), "no change: no new rollout")

		s = getServer(s)
		s.Spec.MySQLConfig["max_connections"] = "700"
		Expect(k8sClient.Update(ctx, s)).To(Succeed())
		reconcileServer(r, s)
		Expect(hash()).NotTo(Equal(first))
	})

	It("adds the restore init container and a persistent volume claim", func() {
		s := baseServer(ns, "shop")
		s.Spec.Storage = vtv1.StorageSpec{Type: vtv1.StoragePersistent}
		s.Spec.ServiceAccountName = "snapshots"
		s.Spec.RestoreFrom = &vtv1.RestoreSpec{Tag: "latest", S3: vtv1.S3Spec{
			Bucket: "bkt", Prefix: "pre", Endpoint: "http://silo:9000", ForcePathStyle: true,
			CredentialsSecretRef: &corev1.LocalObjectReference{Name: "creds"},
		}}
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		reconcileServer(r, s)

		var sts appsv1.StatefulSet
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "shop"}, &sts)).To(Succeed())
		Expect(sts.Spec.VolumeClaimTemplates).To(HaveLen(1))
		Expect(sts.Spec.Template.Spec.ServiceAccountName).To(Equal("snapshots"))
		init := sts.Spec.Template.Spec.InitContainers
		Expect(init).To(HaveLen(1), "no mysqlConfig: only the restore container")
		Expect(init[0].Image).To(Equal("datactl:test"))
		Expect(init[0].Args).To(ContainElements("restore", "--bucket", "bkt", "--prefix", "pre", "--endpoint", "http://silo:9000",
			"--force-path-style", "--tag", "latest", "--pin-file", "/etc/vttest/restore-pin/name", "--keyspace", "commerce:1"))
		Expect(init[0].EnvFrom[0].SecretRef.Name).To(Equal("creds"))
		Expect(getServer(s).Status.Phase).To(Equal(vtv1.PhaseRestoring))
	})

	It("runs the restore before the my.cnf step, and mounts the settings in the init container", func() {
		s := baseServer(ns, "shop")
		s.Spec.MySQLConfig = map[string]string{"innodb_buffer_pool_size": "64M"}
		s.Spec.RestoreFrom = &vtv1.RestoreSpec{Tag: "latest", S3: vtv1.S3Spec{Bucket: "bkt"}}
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		reconcileServer(r, s)
		var sts appsv1.StatefulSet
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "shop"}, &sts)).To(Succeed())
		init := sts.Spec.Template.Spec.InitContainers
		Expect(init).To(HaveLen(2))
		Expect(init[0].Name).To(Equal("restore"))
		Expect(init[1].Name).To(Equal("apply-my-cnf"))
		Expect(init[1].VolumeMounts).To(ContainElement(HaveField("Name", "mycnf")))
		Expect(init[1].VolumeMounts).To(ContainElement(HaveField("Name", "data")))
	})

	It("fails a sharded keyspace without a sharded VSchema and creates nothing", func() {
		Expect(k8sClient.Create(ctx, configMap(ns, "vs", map[string]string{"customer.json": `{}`}))).To(Succeed())
		s := baseServer(ns, "shop")
		s.Spec.Keyspaces = []vtv1.KeyspaceSpec{{Name: "customer", Shards: 2}}
		s.Spec.VSchema = &vtv1.VSchemaSource{ConfigMapRef: corev1.LocalObjectReference{Name: "vs"}}
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		reconcileServer(r, s)

		got := getServer(s)
		Expect(got.Status.Phase).To(Equal(vtv1.PhaseFailed))
		Expect(got.Status.Conditions[0].Reason).To(Equal(vtv1.ReasonVSchemaMissing))
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "shop"}, &appsv1.StatefulSet{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("fails on an invalid VSchema and does not retry", func() {
		Expect(k8sClient.Create(ctx, configMap(ns, "vs", map[string]string{"commerce.json": `{"tabels": {}}`}))).To(Succeed())
		s := baseServer(ns, "shop")
		s.Spec.VSchema = &vtv1.VSchemaSource{ConfigMapRef: corev1.LocalObjectReference{Name: "vs"}}
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		res := reconcileServer(r, s)
		Expect(res.RequeueAfter).To(BeZero())
		got := getServer(s)
		Expect(got.Status.Phase).To(Equal(vtv1.PhaseFailed))
		Expect(got.Status.Conditions[0].Reason).To(Equal("VSchemaInvalid"))
	})

	It("waits for a missing ConfigMap and applies once it exists", func() {
		s := baseServer(ns, "shop")
		s.Spec.VSchema = &vtv1.VSchemaSource{ConfigMapRef: corev1.LocalObjectReference{Name: "vs"}}
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		reconcileServer(r, s)
		pod := makePod(s, true, nil)

		reconcileServer(r, s)
		got := getServer(s)
		Expect(got.Status.Phase).To(Equal(vtv1.PhaseConfiguring))
		Expect(got.Status.Conditions[0].Reason).To(Equal("InputsMissing"))
		Expect(f.calls()).To(BeZero())

		Expect(k8sClient.Create(ctx, configMap(ns, "vs", map[string]string{"commerce.json": commerceVSchema}))).To(Succeed())
		reconcileServer(r, s)
		got = getServer(s)
		Expect(got.Status.Phase).To(Equal(vtv1.PhaseReady))
		Expect(got.Status.AppliedPodUID).To(Equal(string(pod.UID)))
		Expect(f.vschemas).To(Equal([]string{"commerce"}))
	})

	It("applies the VSchema, then init SQL, once per pod and per input change", func() {
		Expect(k8sClient.Create(ctx, configMap(ns, "vs", map[string]string{
			"commerce.json": commerceVSchema, "customer.json": customerVSchema,
		}))).To(Succeed())
		Expect(k8sClient.Create(ctx, configMap(ns, "sql", map[string]string{
			"10-seq.sql": "create table if not exists customer_seq(id int); insert ignore into customer_seq values (0);",
		}))).To(Succeed())
		s := baseServer(ns, "shop")
		s.Spec.Keyspaces = []vtv1.KeyspaceSpec{{Name: "commerce", Shards: 1}, {Name: "customer", Shards: 2}}
		s.Spec.VSchema = &vtv1.VSchemaSource{ConfigMapRef: corev1.LocalObjectReference{Name: "vs"}}
		s.Spec.InitSQL = []vtv1.InitSQLSource{{Keyspace: "commerce", ConfigMapKeyRef: vtv1.ConfigMapKeyRef{Name: "sql", Key: "10-seq.sql"}}}
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		reconcileServer(r, s)
		pod := makePod(s, false, nil)

		reconcileServer(r, s)
		Expect(getServer(s).Status.Phase).To(Equal(vtv1.PhaseStarting))
		Expect(f.calls()).To(BeZero())

		setPodStatus(pod, true, nil)
		reconcileServer(r, s)
		got := getServer(s)
		Expect(got.Status.Phase).To(Equal(vtv1.PhaseReady))
		Expect(f.vschemas).To(Equal([]string{"commerce", "customer"}))
		Expect(f.sql).To(Equal([]string{"commerce: create table if not exists customer_seq(id int) | insert ignore into customer_seq values (0)"}))
		Expect(got.Status.VSchema.AppliedHash).To(HavePrefix("sha256:"))

		By("a second reconcile does nothing")
		before := f.calls()
		reconcileServer(r, s)
		Expect(f.calls()).To(Equal(before))

		By("a changed ConfigMap is applied again")
		cm := &corev1.ConfigMap{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "vs"}, cm)).To(Succeed())
		cm.Data["commerce.json"] = `{"tables": {"product": {}}}`
		Expect(k8sClient.Update(ctx, cm)).To(Succeed())
		reconcileServer(r, s)
		// A change applies every VSchema and every SQL file again, which is safe because both are idempotent.
		Expect(f.calls()).To(Equal(before + 3))

		By("a new pod gets everything again")
		Expect(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0))).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{}))
		}).Should(BeTrue())
		makePod(s, true, nil)
		reconcileServer(r, s)
		Expect(f.calls()).To(Equal(before + 3 + 3))
	})

	It("retries a failed apply after a delay and keeps the server not Ready", func() {
		Expect(k8sClient.Create(ctx, configMap(ns, "vs", map[string]string{"commerce.json": commerceVSchema}))).To(Succeed())
		s := baseServer(ns, "shop")
		s.Spec.VSchema = &vtv1.VSchemaSource{ConfigMapRef: corev1.LocalObjectReference{Name: "vs"}}
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		reconcileServer(r, s)
		makePod(s, true, nil)

		f.failWith = fmt.Errorf("rpc error: connection refused")
		res := reconcileServer(r, s)
		Expect(res.RequeueAfter).NotTo(BeZero())
		got := getServer(s)
		Expect(got.Status.Phase).To(Equal(vtv1.PhaseConfiguring))
		Expect(got.Status.Conditions[0].Reason).To(Equal("ApplyFailed"))

		f.failWith = nil
		reconcileServer(r, s)
		Expect(getServer(s).Status.Phase).To(Equal(vtv1.PhaseReady))
	})

	It("pins the snapshot after the restore init container finished", func() {
		s := baseServer(ns, "shop")
		s.Spec.RestoreFrom = &vtv1.RestoreSpec{Tag: "latest", S3: vtv1.S3Spec{Bucket: "bkt"}}
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		reconcileServer(r, s)
		pod := makePod(s, false, func(p *corev1.Pod) {
			p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: restoreContainer,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
		})
		reconcileServer(r, s)
		Expect(getServer(s).Status.Phase).To(Equal(vtv1.PhaseRestoring))
		Expect(getServer(s).Status.RestoredFrom).To(BeNil())

		msg, _ := json.Marshal(manifest.TerminationMessage{Name: "golden-1", SHA256: "abc"})
		setPodStatus(pod, false, func(p *corev1.Pod) {
			p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: restoreContainer,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: string(msg)}}}}
		})
		reconcileServer(r, s)
		got := getServer(s)
		Expect(got.Status.RestoredFrom).NotTo(BeNil())
		Expect(got.Status.RestoredFrom.Name).To(Equal("golden-1"))
		Expect(got.Status.Phase).To(Equal(vtv1.PhaseStarting))
		var pin corev1.ConfigMap
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "shop-restore-pin"}, &pin)).To(Succeed())
		Expect(pin.Data["name"]).To(Equal("golden-1"))
	})

	It("fails with the init container error when the restore is refused", func() {
		s := baseServer(ns, "shop")
		s.Spec.RestoreFrom = &vtv1.RestoreSpec{Name: "golden-1", S3: vtv1.S3Spec{Bucket: "bkt"}}
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		reconcileServer(r, s)
		msg, _ := json.Marshal(manifest.TerminationMessage{Error: "fingerprint mismatch: keyspace \"customer\" has 2 shards in the snapshot, spec wants 3"})
		makePod(s, false, func(p *corev1.Pod) {
			p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: restoreContainer,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: string(msg)}}}}
		})
		reconcileServer(r, s)
		got := getServer(s)
		Expect(got.Status.Phase).To(Equal(vtv1.PhaseFailed))
		var restored *metav1.Condition
		for i := range got.Status.Conditions {
			if got.Status.Conditions[i].Type == vtv1.ConditionRestored {
				restored = &got.Status.Conditions[i]
			}
		}
		Expect(restored).NotTo(BeNil())
		Expect(restored.Message).To(ContainSubstring("fingerprint mismatch"))
	})

	It("reports a failed restore also after the snapshot was pinned", func() {
		s := baseServer(ns, "shop")
		s.Spec.RestoreFrom = &vtv1.RestoreSpec{Tag: "latest", S3: vtv1.S3Spec{Bucket: "bkt"}}
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		reconcileServer(r, s)
		ok, _ := json.Marshal(manifest.TerminationMessage{Name: "golden-1", SHA256: "abc"})
		pod := makePod(s, false, func(p *corev1.Pod) {
			p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: restoreContainer,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Message: string(ok)}}}}
		})
		reconcileServer(r, s)
		Expect(getServer(s).Status.RestoredFrom).NotTo(BeNil())

		// The pod is made again (a lost ephemeral volume) and the pinned snapshot is gone from the store.
		Expect(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0))).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{}))
		}).Should(BeTrue())
		bad, _ := json.Marshal(manifest.TerminationMessage{Error: `read manifest of "golden-1": not found`})
		makePod(s, false, func(p *corev1.Pod) {
			p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: restoreContainer,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: string(bad)}}}}
		})
		reconcileServer(r, s)
		got := getServer(s)
		Expect(got.Status.Phase).To(Equal(vtv1.PhaseFailed))
		Expect(got.Status.Conditions).To(ContainElement(HaveField("Message", ContainSubstring("not found"))))
	})

	It("says why the server container crashed", func() {
		s := baseServer(ns, "shop")
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		reconcileServer(r, s)
		makePod(s, false, func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: serverContainer, RestartCount: 4,
				State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}}}}
		})
		reconcileServer(r, s)
		got := getServer(s)
		Expect(got.Status.Phase).To(Equal(vtv1.PhaseFailed))
		Expect(got.Status.Conditions[0].Message).To(ContainSubstring("last exit: OOMKilled, code 137, 4 restarts"))
	})

	Describe("a stale snapshot hold", func() {
		holdServer := func(name, holder string) *vtv1.VTTestServer {
			s := baseServer(ns, name)
			s.Annotations = map[string]string{vtv1.SnapshotHoldAnnot: holder}
			Expect(k8sClient.Create(ctx, s)).To(Succeed())
			return s
		}
		replicas := func(s *vtv1.VTTestServer) int32 {
			var sts appsv1.StatefulSet
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(s), &sts)).To(Succeed())
			return ptr.Deref(sts.Spec.Replicas, 1)
		}

		It("is released when the snapshot does not exist", func() {
			s := holdServer("shop", "ghost")
			reconcileServer(r, s)
			Expect(getServer(s).Annotations).NotTo(HaveKey(vtv1.SnapshotHoldAnnot))
			Expect(replicas(s)).To(Equal(int32(1)))
		})

		It("is released when the snapshot has finished, whatever way it ended", func() {
			for i, phase := range []string{vtv1.SnapshotReady, vtv1.SnapshotFailed, vtv1.SnapshotExpired} {
				name := fmt.Sprintf("done-%d", i)
				snap := newSnap(ns, name, "shop"+name)
				Expect(k8sClient.Create(ctx, snap)).To(Succeed())
				snap.Status.Phase = phase
				Expect(k8sClient.Status().Update(ctx, snap)).To(Succeed())
				s := holdServer("shop"+name, name)
				reconcileServer(r, s)
				Expect(getServer(s).Annotations).NotTo(HaveKey(vtv1.SnapshotHoldAnnot), phase)
			}
		})

		It("stays while the snapshot is running", func() {
			for i, phase := range []string{"", vtv1.SnapshotPending, vtv1.SnapshotQuiescing, vtv1.SnapshotUploading} {
				name := fmt.Sprintf("run-%d", i)
				snap := newSnap(ns, name, "shop"+name)
				Expect(k8sClient.Create(ctx, snap)).To(Succeed())
				if phase != "" {
					snap.Status.Phase = phase
					Expect(k8sClient.Status().Update(ctx, snap)).To(Succeed())
				}
				s := holdServer("shop"+name, name)
				reconcileServer(r, s)
				Expect(getServer(s).Annotations).To(HaveKey(vtv1.SnapshotHoldAnnot), "phase %q", phase)
				Expect(replicas(s)).To(Equal(int32(0)))
			}
		})

		It("maps a snapshot to its server", func() {
			snap := newSnap(ns, "x", "shop")
			reqs := snapshotToServer(ctx, snap)
			Expect(reqs).To(HaveLen(1))
			Expect(reqs[0].Name).To(Equal("shop"))
			Expect(reqs[0].Namespace).To(Equal(ns))
		})
	})

	It("scales to zero while a snapshot holds the server", func() {
		s := baseServer(ns, "shop")
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		reconcileServer(r, s)

		// The hold only counts while its snapshot runs: a hold without a snapshot is released.
		snap := newSnap(ns, "snap-1", "shop")
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		snap.Status.Phase = vtv1.SnapshotQuiescing
		Expect(k8sClient.Status().Update(ctx, snap)).To(Succeed())
		s = getServer(s)
		s.Annotations = map[string]string{vtv1.SnapshotHoldAnnot: "snap-1"}
		Expect(k8sClient.Update(ctx, s)).To(Succeed())
		reconcileServer(r, s)
		var sts appsv1.StatefulSet
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "shop"}, &sts)).To(Succeed())
		Expect(ptr.Deref(sts.Spec.Replicas, 1)).To(Equal(int32(0)))
		Expect(getServer(s).Status.Phase).To(Equal(vtv1.PhaseSuspended))

		s = getServer(s)
		delete(s.Annotations, vtv1.SnapshotHoldAnnot)
		Expect(k8sClient.Update(ctx, s)).To(Succeed())
		reconcileServer(r, s)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "shop"}, &sts)).To(Succeed())
		Expect(*sts.Spec.Replicas).To(Equal(int32(1)))
	})

	It("maps a ConfigMap to the servers that read it", func() {
		s := baseServer(ns, "shop")
		s.Spec.VSchema = &vtv1.VSchemaSource{ConfigMapRef: corev1.LocalObjectReference{Name: "vs"}}
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		// The index exists only in a manager cache, so test the pod mapping here.
		reqs := podToServer(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "shop-0", Labels: labelsFor(s)}})
		Expect(reqs).To(HaveLen(1))
		Expect(reqs[0].Name).To(Equal("shop"))
		Expect(podToServer(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "other"}})).To(BeEmpty())
	})
})
