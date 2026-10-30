package controller

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	vtv1 "github.com/likesavabutworse/vttestserver-operator/api/v1alpha1"
)

// gaugeValue returns the value of the series of a metric family with the given label values, or -1.
func gaugeValue(reg *prometheus.Registry, name string, labels map[string]string) float64 {
	families, err := reg.Gather()
	Expect(err).NotTo(HaveOccurred())
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if matches(m, labels) {
				return m.GetGauge().GetValue()
			}
		}
	}
	return -1
}

func matches(m *dto.Metric, want map[string]string) bool {
	got := map[string]string{}
	for _, l := range m.GetLabel() {
		got[l.GetName()] = l.GetValue()
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

var _ = Describe("operator metrics", func() {
	It("counts servers and snapshots by phase and reports the age of the oldest phase", func() {
		ns := newNamespace()
		persistentReadyServer(ns, "ready")
		failed := baseServer(ns, "failed")
		Expect(k8sClient.Create(ctx, failed)).To(Succeed())
		failed.Status.Phase = vtv1.PhaseFailed
		Expect(k8sClient.Status().Update(ctx, failed)).To(Succeed())

		snap := newSnap(ns, "snap-1", "ready")
		Expect(k8sClient.Create(ctx, snap)).To(Succeed())
		started := metav1.NewTime(time.Now().Add(-90 * time.Second))
		snap.Status.Phase = vtv1.SnapshotUploading
		snap.Status.PhaseStartedAt = &started
		Expect(k8sClient.Status().Update(ctx, snap)).To(Succeed())

		reg := prometheus.NewPedanticRegistry()
		reg.MustRegister(NewStateCollector(k8sClient))

		// Other specs share the API server, so check at least the objects of this spec.
		Expect(gaugeValue(reg, "vttestserver_operator_servers", map[string]string{"phase": "Failed"})).To(BeNumerically(">=", 1))
		Expect(gaugeValue(reg, "vttestserver_operator_servers", map[string]string{"phase": "Ready"})).To(BeNumerically(">=", 1))
		Expect(gaugeValue(reg, "vttestserver_operator_servers", map[string]string{"phase": "Restoring"})).To(BeNumerically(">=", 0))
		Expect(gaugeValue(reg, "vttestserver_operator_snapshots", map[string]string{"phase": "Uploading"})).To(BeNumerically(">=", 1))
		Expect(gaugeValue(reg, "vttestserver_operator_snapshot_oldest_phase_age_seconds", map[string]string{"phase": "Uploading"})).
			To(BeNumerically(">=", 90))
		Expect(gaugeValue(reg, "vttestserver_operator_snapshot_oldest_phase_age_seconds", map[string]string{"phase": "Quiescing"})).
			To(BeNumerically(">=", 0), "every active phase has a series")
		Expect(gaugeValue(reg, "vttestserver_operator_snapshot_oldest_phase_age_seconds", map[string]string{"phase": "Ready"})).
			To(BeNumerically("==", -1), "a final phase has no age")
	})

	It("records the duration of a finished snapshot and counts failed snapshots", func() {
		sumBefore := summarySum(snapshotDuration)
		failedBefore := testutil.ToFloat64(snapshotFailures)

		observeSnapshotDone(&vtv1.VTTestServerSnapshot{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(time.Now().Add(-40 * time.Second))}}, time.Now())
		Expect(summarySum(snapshotDuration) - sumBefore).To(BeNumerically("~", 40, 1))

		snapshotFailures.Inc()
		Expect(testutil.ToFloat64(snapshotFailures) - failedBefore).To(BeNumerically("==", 1))
	})
})

func summarySum(s prometheus.Summary) float64 {
	m := &dto.Metric{}
	Expect(s.Write(m)).To(Succeed())
	return m.GetSummary().GetSampleSum()
}
