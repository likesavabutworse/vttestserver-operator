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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	vtv1 "github.com/likesavabutworse/vttestserver-operator/api/v1alpha1"
	"github.com/likesavabutworse/vttestserver-operator/internal/snapshot/manifest"
	"github.com/likesavabutworse/vttestserver-operator/internal/vitess"
)

// configMapIndex indexes VTTestServers by the names of the ConfigMaps they read.
const configMapIndex = ".spec.configMapNames"

// VitessClient is the part of the vtctld client that the controller uses.
type VitessClient interface {
	ApplyVSchema(ctx context.Context, keyspace string, vschemaJSON []byte) error
	ExecSQL(ctx context.Context, keyspace string, statements []string) error
	Close() error
}

// VitessDialer connects to the vtctld gRPC address and the vtgate MySQL address of a server.
type VitessDialer func(ctx context.Context, grpcAddr, mysqlAddr string) (VitessClient, error)

// DefaultDialer dials with the real Vitess client.
func DefaultDialer(ctx context.Context, grpcAddr, mysqlAddr string) (VitessClient, error) {
	return vitess.Dial(ctx, grpcAddr, mysqlAddr)
}

// VTTestServerReconciler reconciles a VTTestServer object
type VTTestServerReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	// DatactlImage is the image of the restore init container.
	DatactlImage string
	// Dial connects to vtctld. DefaultDialer when nil.
	Dial VitessDialer
	// MaxConcurrentReconciles is the number of servers reconciled at the same time. 1 when 0.
	MaxConcurrentReconciles int
}

// +kubebuilder:rbac:groups=vttestserver-operator.io,resources=vttestservers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=vttestserver-operator.io,resources=vttestservers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=vttestserver-operator.io,resources=vttestservers/finalizers,verbs=update
// +kubebuilder:rbac:groups=vttestserver-operator.io,resources=vttestserversnapshots,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// inputs are the VSchema and init SQL files, read from ConfigMaps.
type inputs struct {
	vschema     map[string][]byte // keyspace -> JSON
	sql         []sqlFile
	vschemaHash string
	sqlHash     string
	missing     string // set when a referenced ConfigMap or key does not exist yet
}

type sqlFile struct {
	keyspace string
	text     string
}

// permanentError is an error that retrying cannot fix. Only a change of the spec or of an input does.
type permanentError struct {
	reason string
	err    error
}

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

func (r *VTTestServerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var s vtv1.VTTestServer
	if err := r.Get(ctx, req.NamespacedName, &s); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !s.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	orig := s.DeepCopy()
	res, err := r.reconcile(ctx, &s)
	if apierrors.IsConflict(err) {
		// A child changed under us. The next pass sees the new version.
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	s.Status.ObservedGeneration = s.Generation
	if orig.Status.Phase != s.Status.Phase {
		logf.FromContext(ctx).Info("phase changed", "from", orig.Status.Phase, "to", s.Status.Phase, "message", readyMessage(&s))
	}
	if !equality.Semantic.DeepEqual(orig.Status, s.Status) {
		if perr := r.Status().Patch(ctx, &s, client.MergeFrom(orig)); perr != nil {
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

func (r *VTTestServerReconciler) reconcile(ctx context.Context, s *vtv1.VTTestServer) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	s.Status.Endpoints = vtv1.Endpoints{MySQL: mysqlAddress(s), GRPC: grpcAddress(s)}

	in, err := r.loadInputs(ctx, s)
	if perm, ok := errors.AsType[*permanentError](err); ok {
		r.fail(s, perm.reason, perm.err.Error())
		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.releaseStaleHold(ctx, s); err != nil {
		return ctrl.Result{}, err
	}
	held := s.Annotations[vtv1.SnapshotHoldAnnot] != ""
	replicas := int32(1)
	if held {
		replicas = 0
	}
	if err := r.ensureChildren(ctx, s, replicas); err != nil {
		return ctrl.Result{}, err
	}

	pod, err := r.getPod(ctx, s)
	if err != nil {
		return ctrl.Result{}, err
	}

	if held {
		s.Status.Phase = vtv1.PhaseSuspended
		s.Status.AppliedPodUID = ""
		setCondition(s, vtv1.ConditionReady, metav1.ConditionFalse, "Suspended", "stopped for snapshot "+s.Annotations[vtv1.SnapshotHoldAnnot])
		return ctrl.Result{}, nil
	}

	if s.Spec.RestoreFrom != nil {
		if done, err := r.trackRestore(ctx, s, pod); err != nil {
			return ctrl.Result{}, err
		} else if !done {
			return ctrl.Result{}, nil
		}
	}

	switch {
	case pod == nil:
		s.Status.Phase = vtv1.PhasePending
		setCondition(s, vtv1.ConditionReady, metav1.ConditionFalse, "PodMissing", "waiting for the pod")
		return ctrl.Result{}, nil
	case crashLooping(pod):
		r.fail(s, "CrashLoop", "vttestserver container is in CrashLoopBackOff"+lastExit(pod)+": see the pod logs")
		return ctrl.Result{}, nil
	case !podReady(pod):
		s.Status.Phase = vtv1.PhaseStarting
		setCondition(s, vtv1.ConditionReady, metav1.ConditionFalse, "NotReady", "waiting for vtgate to accept connections")
		return ctrl.Result{}, nil
	}

	if in.missing != "" {
		s.Status.Phase = vtv1.PhaseConfiguring
		setCondition(s, vtv1.ConditionReady, metav1.ConditionFalse, "InputsMissing", in.missing)
		return ctrl.Result{}, nil
	}

	uptodate := s.Status.AppliedPodUID == string(pod.UID) &&
		s.Status.VSchema.AppliedHash == in.vschemaHash &&
		s.Status.InitSQL.AppliedHash == in.sqlHash
	if !uptodate {
		s.Status.Phase = vtv1.PhaseConfiguring
		if err := r.apply(ctx, s, in); err != nil {
			log.Error(err, "apply VSchema or init SQL")
			setCondition(s, vtv1.ConditionReady, metav1.ConditionFalse, "ApplyFailed", err.Error())
			r.Recorder.Eventf(s, nil, corev1.EventTypeWarning, "ApplyFailed", "Apply", "%s", err.Error())
			// vtctld may need seconds after the port opens, so retry.
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		log.Info("applied VSchema and init SQL", "vschemaHash", in.vschemaHash, "sqlHash", in.sqlHash,
			"vschemas", len(in.vschema), "sqlFiles", len(in.sql), "pod", pod.Name)
		now := metav1.Now()
		s.Status.AppliedPodUID = string(pod.UID)
		s.Status.VSchema = vtv1.AppliedState{AppliedHash: in.vschemaHash, AppliedAt: &now}
		s.Status.InitSQL = vtv1.AppliedState{AppliedHash: in.sqlHash, AppliedAt: &now}
		setCondition(s, vtv1.ConditionVSchemaApplied, metav1.ConditionTrue, "Applied", fmt.Sprintf("%d keyspace VSchemas applied", len(in.vschema)))
		setCondition(s, vtv1.ConditionInitSQLApplied, metav1.ConditionTrue, "Applied", fmt.Sprintf("%d SQL files applied", len(in.sql)))
	}
	s.Status.Phase = vtv1.PhaseReady
	setCondition(s, vtv1.ConditionReady, metav1.ConditionTrue, "Ready", "vtgate accepts connections, VSchema and init SQL are applied")
	return ctrl.Result{}, nil
}

func readyMessage(s *vtv1.VTTestServer) string {
	if c := meta.FindStatusCondition(s.Status.Conditions, vtv1.ConditionReady); c != nil {
		return c.Reason + ": " + c.Message
	}
	return ""
}

// releaseStaleHold removes a snapshot hold whose snapshot is gone or finished. The snapshot controller
// releases the hold itself, but a crash or a failed patch at the wrong moment must not leave a server
// stopped for good.
func (r *VTTestServerReconciler) releaseStaleHold(ctx context.Context, s *vtv1.VTTestServer) error {
	holder := s.Annotations[vtv1.SnapshotHoldAnnot]
	if holder == "" {
		return nil
	}
	var snap vtv1.VTTestServerSnapshot
	err := r.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: holder}, &snap)
	stale := apierrors.IsNotFound(err)
	if err != nil && !stale {
		return err
	}
	if err == nil {
		switch snap.Status.Phase {
		case vtv1.SnapshotReady, vtv1.SnapshotFailed, vtv1.SnapshotExpired, vtv1.SnapshotDeleting:
			stale = true
		}
		if !snap.DeletionTimestamp.IsZero() {
			stale = true
		}
	}
	if !stale {
		return nil
	}
	logf.FromContext(ctx).Info("released a stale snapshot hold", "snapshot", holder)
	patch := client.MergeFrom(s.DeepCopy())
	delete(s.Annotations, vtv1.SnapshotHoldAnnot)
	return r.Patch(ctx, s, patch)
}

func (r *VTTestServerReconciler) fail(s *vtv1.VTTestServer, reason, msg string) {
	s.Status.Phase = vtv1.PhaseFailed
	setCondition(s, vtv1.ConditionReady, metav1.ConditionFalse, reason, msg)
	r.Recorder.Eventf(s, nil, corev1.EventTypeWarning, reason, "Reconcile", "%s", msg)
}

func setCondition(s *vtv1.VTTestServer, t string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{
		Type: t, Status: status, Reason: reason, Message: msg, ObservedGeneration: s.Generation,
	})
}

func (r *VTTestServerReconciler) getPod(ctx context.Context, s *vtv1.VTTestServer) (*corev1.Pod, error) {
	var pod corev1.Pod
	err := r.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: podName(s)}, &pod)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !pod.DeletionTimestamp.IsZero() {
		return nil, nil
	}
	return &pod, nil
}

func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// lastExit says why the server container died the last time, for example OOMKilled.
func lastExit(pod *corev1.Pod) string {
	for _, c := range pod.Status.ContainerStatuses {
		if c.Name != serverContainer {
			continue
		}
		if t := c.LastTerminationState.Terminated; t != nil {
			reason := t.Reason
			if reason == "" {
				reason = "Error"
			}
			return fmt.Sprintf(" (last exit: %s, code %d, %d restarts)", reason, t.ExitCode, c.RestartCount)
		}
	}
	return ""
}

func crashLooping(pod *corev1.Pod) bool {
	for _, c := range pod.Status.ContainerStatuses {
		if c.Name == serverContainer && c.State.Waiting != nil && c.State.Waiting.Reason == "CrashLoopBackOff" {
			return true
		}
	}
	return false
}

// trackRestore follows the restore init container. It pins the snapshot that the
// restore used. It returns true when the pod can go on to the start phases.
func (r *VTTestServerReconciler) trackRestore(ctx context.Context, s *vtv1.VTTestServer, pod *corev1.Pod) (bool, error) {
	if s.Status.RestoredFrom != nil {
		if err := r.ensurePin(ctx, s); err != nil {
			return false, err
		}
		// A pod that is made again (a lost ephemeral volume, for example) restores the pinned
		// snapshot again. If that fails, say so instead of waiting for vtgate forever.
		if pod != nil {
			if code, msg, terminated := restoreOutcome(pod); terminated && code != 0 {
				r.failRestore(s, code, msg)
				return false, nil
			}
		}
		return true, nil
	}
	if pod == nil {
		s.Status.Phase = vtv1.PhaseRestoring
		setCondition(s, vtv1.ConditionRestored, metav1.ConditionFalse, "Pending", "waiting for the pod")
		setCondition(s, vtv1.ConditionReady, metav1.ConditionFalse, "Restoring", "waiting for the pod")
		return false, nil
	}
	code, msg, terminated := restoreOutcome(pod)
	switch {
	case terminated && code == 0 && msg.Name != "":
		s.Status.RestoredFrom = &vtv1.RestoredFrom{Name: msg.Name, SHA256: msg.SHA256, ResolvedAt: metav1.Now()}
		setCondition(s, vtv1.ConditionRestored, metav1.ConditionTrue, "Restored", "restored snapshot "+msg.Name)
		r.Recorder.Eventf(s, nil, corev1.EventTypeNormal, "Restored", "Restore", "restored snapshot %s", msg.Name)
		if err := r.ensurePin(ctx, s); err != nil {
			return false, err
		}
		return true, nil
	case terminated && code != 0:
		r.failRestore(s, code, msg)
		return false, nil
	default:
		s.Status.Phase = vtv1.PhaseRestoring
		setCondition(s, vtv1.ConditionRestored, metav1.ConditionFalse, "InProgress", "the restore init container is running")
		setCondition(s, vtv1.ConditionReady, metav1.ConditionFalse, "Restoring", "restoring a snapshot")
		return false, nil
	}
}

func (r *VTTestServerReconciler) failRestore(s *vtv1.VTTestServer, code int32, msg manifest.TerminationMessage) {
	text := msg.Error
	if text == "" {
		text = fmt.Sprintf("restore init container exited with code %d: see its logs", code)
	}
	setCondition(s, vtv1.ConditionRestored, metav1.ConditionFalse, vtv1.ReasonRestoreFailed, text)
	r.fail(s, vtv1.ReasonRestoreFailed, text)
}

func restoreOutcome(pod *corev1.Pod) (code int32, msg manifest.TerminationMessage, terminated bool) {
	for _, c := range pod.Status.InitContainerStatuses {
		if c.Name != restoreContainer {
			continue
		}
		t := c.State.Terminated
		if t == nil && c.State.Waiting != nil {
			t = c.LastTerminationState.Terminated
		}
		if t == nil {
			return 0, msg, false
		}
		_ = json.Unmarshal([]byte(t.Message), &msg)
		return t.ExitCode, msg, true
	}
	return 0, msg, false
}

func (r *VTTestServerReconciler) ensurePin(ctx context.Context, s *vtv1.VTTestServer) error {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: pinConfigMapName(s), Namespace: s.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = labelsFor(s)
		cm.Data = map[string]string{"name": s.Status.RestoredFrom.Name}
		return controllerutil.SetControllerReference(s, cm, r.Scheme)
	})
	return err
}

func (r *VTTestServerReconciler) ensureChildren(ctx context.Context, s *vtv1.VTTestServer, replicas int32) error {
	svcs := []*corev1.Service{
		buildService(s, s.Name, "mysql", mysqlPort),
		buildService(s, grpcServiceName(s), "grpc", grpcPort),
	}
	for _, want := range svcs {
		svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: want.Name, Namespace: want.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
			svc.Labels = want.Labels
			svc.Spec.Selector = want.Spec.Selector
			svc.Spec.Ports = want.Spec.Ports
			return controllerutil.SetControllerReference(s, svc, r.Scheme)
		}); err != nil {
			return err
		}
	}

	if len(s.Spec.MySQLConfig) > 0 {
		want := buildMyCnf(s)
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: want.Name, Namespace: want.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
			cm.Labels = want.Labels
			cm.Data = want.Data
			return controllerutil.SetControllerReference(s, cm, r.Scheme)
		}); err != nil {
			return err
		}
	}

	want := buildStatefulSet(s, r.DatactlImage, replicas)
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: want.Name, Namespace: want.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sts, func() error {
		created := sts.CreationTimestamp.IsZero()
		sts.Labels = want.Labels
		sts.Spec.Replicas = want.Spec.Replicas
		sts.Spec.Template = want.Spec.Template
		if created {
			// These fields cannot change on an existing StatefulSet.
			sts.Spec.ServiceName = want.Spec.ServiceName
			sts.Spec.Selector = want.Spec.Selector
			sts.Spec.VolumeClaimTemplates = want.Spec.VolumeClaimTemplates
			sts.Spec.PersistentVolumeClaimRetentionPolicy = want.Spec.PersistentVolumeClaimRetentionPolicy
		}
		return controllerutil.SetControllerReference(s, sts, r.Scheme)
	})
	return err
}

// loadInputs reads the VSchema and init SQL ConfigMaps and checks them.
func (r *VTTestServerReconciler) loadInputs(ctx context.Context, s *vtv1.VTTestServer) (*inputs, error) {
	in := &inputs{vschema: map[string][]byte{}}
	cms := map[string]*corev1.ConfigMap{}
	get := func(name string) (*corev1.ConfigMap, error) {
		if cm, ok := cms[name]; ok {
			return cm, nil
		}
		var cm corev1.ConfigMap
		err := r.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: name}, &cm)
		if apierrors.IsNotFound(err) {
			cms[name] = nil
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		cms[name] = &cm
		return &cm, nil
	}

	if s.Spec.VSchema != nil {
		name := s.Spec.VSchema.ConfigMapRef.Name
		cm, err := get(name)
		if err != nil {
			return nil, err
		}
		if cm == nil {
			in.missing = fmt.Sprintf("ConfigMap %q with the VSchema does not exist yet", name)
		} else {
			for _, ks := range s.Spec.Keyspaces {
				raw, ok := cm.Data[ks.Name+".json"]
				if !ok {
					continue
				}
				if _, err := vitess.ParseVSchema([]byte(raw)); err != nil {
					return nil, &permanentError{reason: "VSchemaInvalid", err: fmt.Errorf("ConfigMap %s key %s.json: %w", name, ks.Name, err)}
				}
				in.vschema[ks.Name] = []byte(raw)
			}
		}
	}

	// A keyspace with more than one shard needs a sharded VSchema, unless a restore brings one.
	if s.Spec.RestoreFrom == nil && in.missing == "" {
		for _, ks := range s.Spec.Keyspaces {
			if ks.Shards <= 1 {
				continue
			}
			raw, ok := in.vschema[ks.Name]
			if !ok {
				return nil, &permanentError{reason: vtv1.ReasonVSchemaMissing,
					err: fmt.Errorf("keyspace %s has %d shards and needs a VSchema with sharded: true: add the key %s.json", ks.Name, ks.Shards, ks.Name)}
			}
			vs, _ := vitess.ParseVSchema(raw)
			if !vs.GetSharded() {
				return nil, &permanentError{reason: vtv1.ReasonVSchemaMissing,
					err: fmt.Errorf("keyspace %s has %d shards but its VSchema does not set sharded: true", ks.Name, ks.Shards)}
			}
		}
	}

	for _, src := range s.Spec.InitSQL {
		cm, err := get(src.ConfigMapKeyRef.Name)
		if err != nil {
			return nil, err
		}
		if cm == nil {
			in.missing = fmt.Sprintf("ConfigMap %q with init SQL does not exist yet", src.ConfigMapKeyRef.Name)
			continue
		}
		text, ok := cm.Data[src.ConfigMapKeyRef.Key]
		if !ok {
			in.missing = fmt.Sprintf("ConfigMap %q has no key %q", src.ConfigMapKeyRef.Name, src.ConfigMapKeyRef.Key)
			continue
		}
		in.sql = append(in.sql, sqlFile{keyspace: src.Keyspace, text: text})
	}

	in.vschemaHash = hashVSchemas(s, in.vschema)
	in.sqlHash = hashSQL(in.sql)
	return in, nil
}

func hashVSchemas(s *vtv1.VTTestServer, vs map[string][]byte) string {
	if len(vs) == 0 {
		return ""
	}
	h := sha256.New()
	for _, ks := range s.Spec.Keyspaces {
		if raw, ok := vs[ks.Name]; ok {
			_, _ = fmt.Fprintf(h, "%s\x00%d\x00", ks.Name, len(raw))
			h.Write(raw)
		}
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func hashSQL(files []sqlFile) string {
	if len(files) == 0 {
		return ""
	}
	h := sha256.New()
	for _, f := range files {
		_, _ = fmt.Fprintf(h, "%s\x00%d\x00%s\x00", f.keyspace, len(f.text), f.text)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// apply sends the VSchema over the vtctld gRPC port, then runs the init SQL through vtgate.
func (r *VTTestServerReconciler) apply(ctx context.Context, s *vtv1.VTTestServer, in *inputs) error {
	if len(in.vschema) == 0 && len(in.sql) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	dial := r.Dial
	if dial == nil {
		dial = DefaultDialer
	}
	c, err := dial(ctx, grpcAddress(s), mysqlAddress(s))
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	for _, ks := range s.Spec.Keyspaces {
		raw, ok := in.vschema[ks.Name]
		if !ok {
			continue
		}
		if err := c.ApplyVSchema(ctx, ks.Name, raw); err != nil {
			return err
		}
	}
	for _, f := range in.sql {
		if err := c.ExecSQL(ctx, f.keyspace, vitess.SplitStatements(f.text)); err != nil {
			return err
		}
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *VTTestServerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorder("vttestserver-operator")
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &vtv1.VTTestServer{}, configMapIndex, func(o client.Object) []string {
		s := o.(*vtv1.VTTestServer)
		var names []string
		if s.Spec.VSchema != nil {
			names = append(names, s.Spec.VSchema.ConfigMapRef.Name)
		}
		for _, src := range s.Spec.InitSQL {
			names = append(names, src.ConfigMapKeyRef.Name)
		}
		return names
	}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		// Status writes must not trigger a new pass. A spec change bumps the generation, and the
		// snapshot hold is an annotation.
		For(&vtv1.VTTestServer{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}))).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(podToServer)).
		Watches(&vtv1.VTTestServerSnapshot{}, handler.EnqueueRequestsFromMapFunc(snapshotToServer)).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.configMapToServers)).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}).
		Named("vttestserver").
		Complete(r)
}

func podToServer(_ context.Context, o client.Object) []reconcile.Request {
	name := o.GetLabels()[labelServer]
	if name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: o.GetNamespace(), Name: name}}}
}

func snapshotToServer(_ context.Context, o client.Object) []reconcile.Request {
	snap, ok := o.(*vtv1.VTTestServerSnapshot)
	if !ok || snap.Spec.ServerRef.Name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: snap.Namespace, Name: snap.Spec.ServerRef.Name}}}
}

func (r *VTTestServerReconciler) configMapToServers(ctx context.Context, o client.Object) []reconcile.Request {
	var list vtv1.VTTestServerList
	if err := r.List(ctx, &list, client.InNamespace(o.GetNamespace()), client.MatchingFields{configMapIndex: o.GetName()}); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for _, s := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&s)})
	}
	return reqs
}
