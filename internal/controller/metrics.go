package controller

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	vtv1 "github.com/likesavabutworse/vttestserver-operator/api/v1alpha1"
)

const labelPhase = "phase"

// Metrics of the operator, next to the ones that controller-runtime already serves (reconcile counts,
// errors and durations). Labels stay few and fixed: a PR-environment cluster has many short-lived objects.
var (
	// snapshotDuration covers the time from the creation of the object to the finished upload. It includes
	// the wait for a server that is not Ready. Snapshots are rare and only the leader counts them, so a
	// summary gives usable quantiles. A quantile has no value when the window holds no observation.
	snapshotDuration = prometheus.NewSummary(prometheus.SummaryOpts{
		Name:       "vttestserver_operator_snapshot_duration_seconds",
		Help:       "Time from the creation of a VTTestServerSnapshot to a finished upload.",
		Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
		MaxAge:     time.Hour,
	})
	snapshotFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "vttestserver_operator_snapshot_failures_total",
		Help: "Snapshots that ended in the Failed phase.",
	})
)

func init() {
	metrics.Registry.MustRegister(snapshotDuration, snapshotFailures)
}

func observeSnapshotDone(snap *vtv1.VTTestServerSnapshot, now time.Time) {
	snapshotDuration.Observe(now.Sub(snap.CreationTimestamp.Time).Seconds())
}

var (
	serverPhases   = []string{vtv1.PhasePending, vtv1.PhaseRestoring, vtv1.PhaseStarting, vtv1.PhaseConfiguring, vtv1.PhaseReady, vtv1.PhaseSuspended, vtv1.PhaseFailed}
	snapshotPhases = []string{vtv1.SnapshotPending, vtv1.SnapshotQuiescing, vtv1.SnapshotUploading, vtv1.SnapshotReady, vtv1.SnapshotFailed, vtv1.SnapshotDeleting, vtv1.SnapshotExpired}

	// activeSnapshotPhases are the phases that must end. Ready, Failed and Expired are final: their age means nothing.
	activeSnapshotPhases = []string{vtv1.SnapshotPending, vtv1.SnapshotQuiescing, vtv1.SnapshotUploading, vtv1.SnapshotDeleting}

	serversDesc = prometheus.NewDesc("vttestserver_operator_servers",
		"VTTestServer objects by phase.", []string{labelPhase}, nil)
	snapshotsDesc = prometheus.NewDesc("vttestserver_operator_snapshots",
		"VTTestServerSnapshot objects by phase.", []string{labelPhase}, nil)
	snapshotAgeDesc = prometheus.NewDesc("vttestserver_operator_snapshot_oldest_phase_age_seconds",
		"Age of the longest running phase among the snapshots in a phase that must end: Pending, Quiescing, Uploading and Deleting. A snapshot that stays long in one of them is stuck.",
		[]string{labelPhase}, nil)
)

// stateCollector reads the object counts when Prometheus scrapes. This is simpler and safer than gauges
// that the controllers keep up to date: nothing goes stale when an object is deleted or the leader changes.
type stateCollector struct {
	reader client.Reader
}

// NewStateCollector returns a collector for the number of servers and snapshots by phase. It reads from
// the cache of the manager.
func NewStateCollector(reader client.Reader) prometheus.Collector {
	return &stateCollector{reader: reader}
}

func (c *stateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- serversDesc
	ch <- snapshotsDesc
	ch <- snapshotAgeDesc
}

func (c *stateCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var servers vtv1.VTTestServerList
	if err := c.reader.List(ctx, &servers); err == nil {
		counts := map[string]float64{}
		for _, s := range servers.Items {
			counts[phaseOr(s.Status.Phase, vtv1.PhasePending)]++
		}
		for _, p := range serverPhases {
			ch <- prometheus.MustNewConstMetric(serversDesc, prometheus.GaugeValue, counts[p], p)
		}
	}

	var snaps vtv1.VTTestServerSnapshotList
	if err := c.reader.List(ctx, &snaps); err == nil {
		now := time.Now()
		counts := map[string]float64{}
		oldest := map[string]float64{}
		for _, s := range snaps.Items {
			p := phaseOr(s.Status.Phase, vtv1.SnapshotPending)
			counts[p]++
			if s.Status.PhaseStartedAt != nil {
				if age := now.Sub(s.Status.PhaseStartedAt.Time).Seconds(); age > oldest[p] {
					oldest[p] = age
				}
			}
		}
		for _, p := range snapshotPhases {
			ch <- prometheus.MustNewConstMetric(snapshotsDesc, prometheus.GaugeValue, counts[p], p)
		}
		for _, p := range activeSnapshotPhases {
			ch <- prometheus.MustNewConstMetric(snapshotAgeDesc, prometheus.GaugeValue, oldest[p], p)
		}
	}
}

// phaseOr names the phase of an object that the controller has not seen yet.
func phaseOr(phase, fallback string) string {
	if phase == "" {
		return fallback
	}
	return phase
}
