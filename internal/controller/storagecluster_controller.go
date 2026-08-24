// Package controller contains the StorageCluster reconciler.
package controller

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	storagev1alpha1 "github.com/jackal/storage-operator/api/v1alpha1"
	"github.com/jackal/storage-operator/internal/disk"
	"github.com/jackal/storage-operator/internal/migration"
	"github.com/jackal/storage-operator/internal/provisioning"
	"github.com/jackal/storage-operator/internal/rook"
	"github.com/jackal/storage-operator/internal/topology"
)

const fieldOwner = "storage-operator"

// StorageClusterReconciler reconciles a StorageCluster object.
type StorageClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=storage.jackal.io,resources=storageclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.jackal.io,resources=storageclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ceph.rook.io,resources=cephclusters;cephblockpools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch

// Reconcile drives the cluster toward the desired storage topology.
func (r *StorageClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	sc := &storagev1alpha1.StorageCluster{}
	if err := r.Get(ctx, req.NamespacedName, sc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	applyDefaults(sc)

	// 1. Observe topology.
	nodes := &corev1.NodeList{}
	if err := r.List(ctx, nodes); err != nil {
		return ctrl.Result{}, err
	}
	snap := topology.Analyze(nodes.Items)
	sc.Status.NodeCount = snap.TotalNodes
	sc.Status.ReadyNodeCount = snap.ReadyNodes

	// 2. Aggregate detected disks from node annotations.
	disks := aggregateDisks(nodes.Items)
	sc.Status.DetectedDisks = disks

	// 3. Select provisioning source.
	pm := &provisioning.Manager{Client: r.Client}
	decision, err := pm.Select(ctx, sc, disks)
	if err != nil {
		return ctrl.Result{}, err
	}
	sc.Status.ProvisioningSource = string(decision.Source)

	// 4. Decide desired mode and build base params for that mode.
	desired := snap.DesiredMode(sc.Spec.HA.MinNodesForHA)
	params, err := buildParams(sc, decision, desired)
	if err != nil {
		return ctrl.Result{}, err
	}

	// 5. Ensure the CephCluster + pool exist for the *current* posture, then
	//    possibly run promotion.
	requeue := 30 * time.Second
	switch {
	case desired == topology.ModeSingleNode:
		if err := r.ensureSingleNode(ctx, sc, params); err != nil {
			return ctrl.Result{}, err
		}
		sc.Status.Phase = storagev1alpha1.PhaseSingleNode
		sc.Status.CurrentReplicas = 1
		sc.Status.DataProtected = false
		sc.Status.HAEligibleSince = nil
		sc.Status.MigrationStep = ""
	case desired == topology.ModeHA:
		res, err := r.reconcileHA(ctx, sc, params, snap)
		if err != nil {
			return ctrl.Result{}, err
		}
		requeue = res
	}

	// 6. Refresh Ceph health into status.
	if cc, err := rook.GetCephCluster(ctx, r.Client, sc.Name, sc.Spec.CephNamespace); err == nil {
		sc.Status.CephHealth = rook.ReadCephHealth(cc)
	}

	if err := r.Status().Update(ctx, sc); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}
	logger.Info("reconciled", "phase", sc.Status.Phase, "source", sc.Status.ProvisioningSource, "readyNodes", snap.SchedulableStorageNodes)
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// ensureSingleNode brings up Ceph on one node bypassing multi-node defaults.
func (r *StorageClusterReconciler) ensureSingleNode(ctx context.Context, sc *storagev1alpha1.StorageCluster, p rook.ClusterParams) error {
	p.MonCount = 1
	p.MonAllowMultiplePer = true
	p.MgrCount = 1
	p.FailureDomain = "osd"
	p.ReplicaSize = 1
	p.RequireSafeReplica = false

	cc := rook.BuildCephCluster(p)
	if err := rook.Apply(ctx, r.Client, cc, fieldOwner); err != nil {
		return err
	}
	if sc.Spec.Pools.Block {
		pool := rook.BuildBlockPool(rook.PoolName(sc), sc.Spec.CephNamespace, "osd", 1, false)
		if err := rook.Apply(ctx, r.Client, pool, fieldOwner); err != nil {
			return err
		}
	}
	return nil
}

// reconcileHA either promotes from single-node or maintains a steady HA state.
func (r *StorageClusterReconciler) reconcileHA(ctx context.Context, sc *storagev1alpha1.StorageCluster, p rook.ClusterParams, snap topology.Snapshot) (time.Duration, error) {
	// Always ensure the single-node cluster is bootstrapped first. Ceph must be
	// running (as single-node) before we can promote it; this is idempotent and
	// a no-op once the CephCluster already exists.
	if sc.Status.Phase != storagev1alpha1.PhaseHA && sc.Status.MigrationStep == "" {
		if err := r.ensureSingleNode(ctx, sc, p); err != nil {
			return 0, err
		}
		if sc.Status.CurrentReplicas == 0 {
			sc.Status.CurrentReplicas = 1
		}
	}

	// Stabilization window: record eligibility and wait before promoting.
	if sc.Status.HAEligibleSince == nil {
		now := metav1.Now()
		sc.Status.HAEligibleSince = &now
	}
	window := time.Duration(sc.Spec.HA.StabilizationWindowSeconds) * time.Second
	elapsed := time.Since(sc.Status.HAEligibleSince.Time)

	// Already HA and steady.
	if sc.Status.Phase == storagev1alpha1.PhaseHA {
		return 60 * time.Second, nil
	}

	if !sc.Spec.HA.AutoPromote {
		sc.Status.Phase = storagev1alpha1.PhaseSingleNode
		return 60 * time.Second, nil
	}

	if elapsed < window {
		sc.Status.Phase = storagev1alpha1.PhaseSingleNode
		return window - elapsed, nil
	}

	// Run the promotion state machine, one step per reconcile.
	engine := &migration.Engine{
		Client:     r.Client,
		Status:     cephStatusReader{Client: r.Client},
		FieldOwner: fieldOwner,
	}
	// HA-target params for cluster applies during promotion.
	p.MonCount = 3
	p.MonAllowMultiplePer = false
	p.MgrCount = 2
	p.FailureDomain = "host"

	step := migration.Step(sc.Status.MigrationStep)
	if step == "" {
		step = migration.StepPreflight
	}
	sc.Status.Phase = storagev1alpha1.PhasePromoting

	res, err := engine.Execute(ctx, sc, step, p)
	if err != nil {
		return 0, err
	}
	setCondition(sc, "Promotion", res.Advance, string(step), res.Message)

	if res.Advance {
		next := migration.Next(step)
		sc.Status.MigrationStep = string(next)
		if next == migration.StepDone {
			sc.Status.Phase = storagev1alpha1.PhaseHA
			sc.Status.CurrentReplicas = targetReplicas(sc)
			sc.Status.DataProtected = true
			sc.Status.MigrationStep = ""
			return 60 * time.Second, nil
		}
		// Advance quickly to the next step.
		return 5 * time.Second, nil
	}
	// Gated; requeue and retry the same step.
	return 15 * time.Second, nil
}

func buildParams(sc *storagev1alpha1.StorageCluster, d provisioning.Decision, mode topology.Mode) (rook.ClusterParams, error) {
	p := rook.ClusterParams{
		Name:      sc.Name,
		Namespace: sc.Spec.CephNamespace,
		CephImage: sc.Spec.CephImage,
	}
	if err := d.ToClusterParams(&p); err != nil {
		return p, err
	}
	return p, nil
}

func aggregateDisks(nodes []corev1.Node) []storagev1alpha1.DetectedDisk {
	var all []storagev1alpha1.DetectedDisk
	for i := range nodes {
		ann := nodes[i].Annotations[disk.AnnotationKey]
		if ann == "" {
			continue
		}
		ds, err := disk.Decode(ann)
		if err != nil {
			continue
		}
		all = append(all, ds...)
	}
	return all
}

func targetReplicas(sc *storagev1alpha1.StorageCluster) int {
	if sc.Spec.HA.TargetReplicas > 0 {
		return sc.Spec.HA.TargetReplicas
	}
	return 3
}

func setCondition(sc *storagev1alpha1.StorageCluster, condType string, ok bool, reason, msg string) {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	cond := metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: metav1.Now(),
	}
	for i := range sc.Status.Conditions {
		if sc.Status.Conditions[i].Type == condType {
			if sc.Status.Conditions[i].Status == status && sc.Status.Conditions[i].Reason == reason {
				cond.LastTransitionTime = sc.Status.Conditions[i].LastTransitionTime
			}
			sc.Status.Conditions[i] = cond
			return
		}
	}
	sc.Status.Conditions = append(sc.Status.Conditions, cond)
}

func applyDefaults(sc *storagev1alpha1.StorageCluster) {
	if sc.Spec.CephNamespace == "" {
		sc.Spec.CephNamespace = "rook-ceph"
	}
	if sc.Spec.CephImage == "" {
		sc.Spec.CephImage = "quay.io/ceph/ceph:v18.2.4"
	}
	if sc.Spec.Provisioning.Mode == "" {
		sc.Spec.Provisioning.Mode = storagev1alpha1.ProvisioningAuto
	}
	if sc.Spec.HA.MinNodesForHA == 0 {
		sc.Spec.HA.MinNodesForHA = 3
	}
	if sc.Spec.HA.TargetReplicas == 0 {
		sc.Spec.HA.TargetReplicas = 3
	}
	if sc.Spec.HA.StabilizationWindowSeconds == 0 {
		sc.Spec.HA.StabilizationWindowSeconds = 300
	}
}

// SetupWithManager wires the reconciler and node watch into the manager.
func (r *StorageClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.StorageCluster{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.mapNodeToClusters)).
		Complete(r)
}

// mapNodeToClusters enqueues all StorageClusters when a node changes.
func (r *StorageClusterReconciler) mapNodeToClusters(ctx context.Context, _ client.Object) []reconcile.Request {
	list := &storagev1alpha1.StorageClusterList{}
	if err := r.List(ctx, list); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
	}
	return reqs
}
