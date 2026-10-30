package controller

import (
	"context"
	"fmt"
	"runtime"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	vtv1 "github.com/likesavabutworse/vttestserver-operator/api/v1alpha1"
)

// Fifty environments at the same time, in a real manager (caches, field index, watches and workers).
// There are no pods in envtest, so this measures the control plane only: how fast the operator makes the
// objects of 50 servers, and how much memory it needs for that.
var _ = Describe("operator with 50 environments", Ordered, Label("scale"), func() {
	const servers = 50
	var (
		ns      string
		mgr     ctrl.Manager
		stop    context.CancelFunc
		stopped = make(chan struct{})
	)

	BeforeAll(func() {
		ns = newNamespace()
		var err error
		mgr, err = ctrl.NewManager(cfg, ctrl.Options{
			Scheme:     scheme.Scheme,
			Metrics:    metricsserver.Options{BindAddress: "0"},
			Controller: config.Controller{SkipNameValidation: new(true)},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect((&VTTestServerReconciler{
			Client: mgr.GetClient(), Scheme: mgr.GetScheme(), DatactlImage: "datactl:test",
			MaxConcurrentReconciles: 3, Dial: (&fakeVitess{}).dial,
		}).SetupWithManager(mgr)).To(Succeed())
		Expect((&VTTestServerSnapshotReconciler{
			Client: mgr.GetClient(), Scheme: mgr.GetScheme(), DatactlImage: "datactl:test", MaxConcurrentReconciles: 3,
		}).SetupWithManager(mgr)).To(Succeed())

		var mgrCtx context.Context
		mgrCtx, stop = context.WithCancel(ctx)
		go func() {
			defer GinkgoRecover()
			defer close(stopped)
			Expect(mgr.Start(mgrCtx)).To(Succeed())
		}()
		Expect(mgr.GetCache().WaitForCacheSync(mgrCtx)).To(BeTrue())
	})

	AfterAll(func() {
		stop()
		Eventually(stopped, 30*time.Second).Should(BeClosed())
	})

	It("creates the objects of every server within a short time, with little memory", func() {
		Expect(k8sClient.Create(ctx, configMap(ns, "shared-vschema", map[string]string{"commerce.json": "{}"}))).To(Succeed())

		start := time.Now()
		for i := range servers {
			s := baseServer(ns, fmt.Sprintf("pr-%02d", i))
			s.Spec.Storage = vtv1.StorageSpec{Type: vtv1.StoragePersistent}
			s.Spec.MySQLConfig = map[string]string{"max_connections": "600"}
			s.Spec.VSchema = &vtv1.VSchemaSource{ConfigMapRef: corev1.LocalObjectReference{Name: "shared-vschema"}}
			s.Spec.RestoreFrom = &vtv1.RestoreSpec{Tag: "latest", S3: vtv1.S3Spec{Bucket: "bkt"}}
			Expect(k8sClient.Create(ctx, s)).To(Succeed())
		}
		created := time.Since(start)

		Eventually(func(g Gomega) {
			var list appsv1.StatefulSetList
			g.Expect(k8sClient.List(ctx, &list, client.InNamespace(ns))).To(Succeed())
			g.Expect(list.Items).To(HaveLen(servers))
		}, 2*time.Minute, 500*time.Millisecond).Should(Succeed())
		all := time.Since(start)

		var svcs corev1.ServiceList
		Expect(k8sClient.List(ctx, &svcs, client.InNamespace(ns))).To(Succeed())
		Expect(svcs.Items).To(HaveLen(2 * servers))

		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		heapMB := m.HeapAlloc >> 20
		AddReportEntry("scale", fmt.Sprintf("%d servers: created in %s, all StatefulSets after %s, operator heap %d MiB",
			servers, created.Round(time.Millisecond), all.Round(time.Millisecond), heapMB))

		Expect(all).To(BeNumerically("<", time.Minute), "50 servers must not need a minute of reconcile time")
		Expect(heapMB).To(BeNumerically("<", 256), "the heap of the whole test process, with envtest clients")
	})

	It("finds all 50 servers from one ConfigMap through the field index of the manager", func() {
		r := &VTTestServerReconciler{Client: mgr.GetClient()}
		reqs := r.configMapToServers(ctx, &corev1.ConfigMap{ObjectMeta: configMap(ns, "shared-vschema", nil).ObjectMeta})
		Expect(reqs).To(HaveLen(servers))
	})
})
