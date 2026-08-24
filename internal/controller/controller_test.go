package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/jackal/rookpp/api/v1alpha1"
	"github.com/jackal/rookpp/internal/migration"
	"github.com/jackal/rookpp/internal/rook"
)

func doReconcile(t *testing.T, ctx context.Context, r *StorageClusterReconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: name}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func getCephCluster(t *testing.T, ctx context.Context, name, ns string) *unstructured.Unstructured {
	t.Helper()
	u, err := rook.GetCephCluster(ctx, testK8s, name, ns)
	if err != nil {
		t.Fatalf("get cephcluster: %v", err)
	}
	return u
}

// setCephStatus writes a status subresource on the CephCluster to simulate Ceph.
func setCephStatus(t *testing.T, ctx context.Context, name, ns, health, pgState string, mons, osdUp, osdIn int) {
	t.Helper()
	u := getCephCluster(t, ctx, name, ns)
	monMap := map[string]interface{}{}
	for i := 0; i < mons; i++ {
		monMap[string(rune('a'+i))] = map[string]interface{}{"name": string(rune('a' + i))}
	}
	status := map[string]interface{}{
		"ceph": map[string]interface{}{
			"health":  health,
			"pgState": pgState,
			"mons":    monMap,
			"osd": map[string]interface{}{
				"up": int64(osdUp),
				"in": int64(osdIn),
			},
		},
	}
	_ = unstructured.SetNestedField(u.Object, status, "status")
	if err := testK8s.Status().Update(ctx, u); err != nil {
		t.Fatalf("set ceph status: %v", err)
	}
}

func TestSingleNodeBringup(t *testing.T) {
	teardown := setup(t)
	defer teardown()
	ctx := context.Background()
	mkNamespace(t, ctx, "rook-ceph")

	mkNode(t, ctx, "solo", true)
	mkDefaultStorageClass(t, ctx, "standard")
	mkStorageCluster(t, ctx, "s1", nil)

	r := newReconciler()
	doReconcile(t, ctx, r, "s1")

	sc := getStorageCluster(t, ctx, "s1")
	if sc.Status.Phase != storagev1alpha1.PhaseSingleNode {
		t.Fatalf("phase = %q want SingleNode", sc.Status.Phase)
	}
	if sc.Status.CurrentReplicas != 1 {
		t.Fatalf("replicas = %d want 1", sc.Status.CurrentReplicas)
	}
	// With a default StorageClass and no raw disks, source must be StorageClass.
	if sc.Status.ProvisioningSource != "StorageClass" {
		t.Fatalf("source = %q want StorageClass", sc.Status.ProvisioningSource)
	}

	cc := getCephCluster(t, ctx, "s1", "rook-ceph")
	monCount, _, _ := unstructured.NestedInt64(cc.Object, "spec", "mon", "count")
	if monCount != 1 {
		t.Fatalf("mon.count = %d want 1", monCount)
	}
	allowMulti, _, _ := unstructured.NestedBool(cc.Object, "spec", "mon", "allowMultiplePerNode")
	if !allowMulti {
		t.Fatalf("expected allowMultiplePerNode=true in single-node mode")
	}
}

func TestLoopbackFallbackWhenNoStorageClass(t *testing.T) {
	teardown := setup(t)
	defer teardown()
	ctx := context.Background()
	mkNamespace(t, ctx, "rook-ceph")

	mkNode(t, ctx, "solo2", true)
	mkStorageCluster(t, ctx, "s2", nil)

	r := newReconciler()
	doReconcile(t, ctx, r, "s2")

	sc := getStorageCluster(t, ctx, "s2")
	if sc.Status.ProvisioningSource != "Loopback" {
		t.Fatalf("source = %q want Loopback", sc.Status.ProvisioningSource)
	}
}

func TestStabilizationWindowDelaysPromotion(t *testing.T) {
	teardown := setup(t)
	defer teardown()
	ctx := context.Background()
	mkNamespace(t, ctx, "rook-ceph")

	for _, n := range []string{"n1", "n2", "n3"} {
		mkNode(t, ctx, n, true)
	}
	mkDefaultStorageClass(t, ctx, "standard")
	mkStorageCluster(t, ctx, "s3", func(sc *storagev1alpha1.StorageCluster) {
		sc.Spec.HA.StabilizationWindowSeconds = 3600
	})

	r := newReconciler()
	doReconcile(t, ctx, r, "s3")

	sc := getStorageCluster(t, ctx, "s3")
	// Eligible but inside the window -> must not have started promotion.
	if sc.Status.HAEligibleSince == nil {
		t.Fatalf("expected HAEligibleSince to be recorded")
	}
	if sc.Status.Phase == storagev1alpha1.PhasePromoting || sc.Status.Phase == storagev1alpha1.PhaseHA {
		t.Fatalf("phase = %q; promotion should be delayed by window", sc.Status.Phase)
	}
}

func TestPromotionStateMachineReachesHA(t *testing.T) {
	teardown := setup(t)
	defer teardown()
	ctx := context.Background()
	mkNamespace(t, ctx, "rook-ceph")

	for _, n := range []string{"h1", "h2", "h3"} {
		mkNode(t, ctx, n, true)
	}
	mkDefaultStorageClass(t, ctx, "standard")
	mkStorageCluster(t, ctx, "s4", func(sc *storagev1alpha1.StorageCluster) {
		sc.Spec.HA.StabilizationWindowSeconds = 0 // eligible immediately
	})

	r := newReconciler()
	// First reconcile creates the CephCluster and records eligibility.
	doReconcile(t, ctx, r, "s4")
	// Force eligibility into the past so the window (0s) is satisfied.
	sc := getStorageCluster(t, ctx, "s4")
	past := metav1.NewTime(time.Now().Add(-time.Hour))
	sc.Status.HAEligibleSince = &past
	if err := testK8s.Status().Update(ctx, sc); err != nil {
		t.Fatalf("seed eligibility: %v", err)
	}

	// Simulate a healthy converged cluster for every step.
	// Drive the state machine to completion; each reconcile advances one step
	// when its gate passes.
	deadline := 20
	for i := 0; i < deadline; i++ {
		setCephStatus(t, ctx, "s4", "rook-ceph", "HEALTH_OK", "active+clean", 3, 3, 3)
		doReconcile(t, ctx, r, "s4")
		sc = getStorageCluster(t, ctx, "s4")
		if sc.Status.Phase == storagev1alpha1.PhaseHA {
			break
		}
	}

	if sc.Status.Phase != storagev1alpha1.PhaseHA {
		t.Fatalf("did not reach HA; phase=%q step=%q", sc.Status.Phase, sc.Status.MigrationStep)
	}
	if !sc.Status.DataProtected {
		t.Fatalf("expected DataProtected=true at HA")
	}
	if sc.Status.CurrentReplicas != 3 {
		t.Fatalf("replicas = %d want 3", sc.Status.CurrentReplicas)
	}

	// Pool must now be host failure domain, replica 3.
	pool := &unstructured.Unstructured{}
	pool.SetGroupVersionKind(rook.CephBlockPoolGVK)
	if err := testK8s.Get(ctx, client.ObjectKey{Name: rook.PoolName(sc), Namespace: "rook-ceph"}, pool); err != nil {
		t.Fatalf("get pool: %v", err)
	}
	fd, _, _ := unstructured.NestedString(pool.Object, "spec", "failureDomain")
	if fd != "host" {
		t.Fatalf("failureDomain = %q want host", fd)
	}
	size, _, _ := unstructured.NestedInt64(pool.Object, "spec", "replicated", "size")
	if size != 3 {
		t.Fatalf("replica size = %d want 3", size)
	}
}

func TestPromotionBlocksOnUnhealthy(t *testing.T) {
	teardown := setup(t)
	defer teardown()
	ctx := context.Background()
	mkNamespace(t, ctx, "rook-ceph")

	for _, n := range []string{"u1", "u2", "u3"} {
		mkNode(t, ctx, n, true)
	}
	mkDefaultStorageClass(t, ctx, "standard")
	mkStorageCluster(t, ctx, "s5", func(sc *storagev1alpha1.StorageCluster) {
		sc.Spec.HA.StabilizationWindowSeconds = 0
	})

	r := newReconciler()
	doReconcile(t, ctx, r, "s5")
	sc := getStorageCluster(t, ctx, "s5")
	past := metav1.NewTime(time.Now().Add(-time.Hour))
	sc.Status.HAEligibleSince = &past
	if err := testK8s.Status().Update(ctx, sc); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Preflight passes (needs clean PGs), then keep cluster in HEALTH_ERR.
	setCephStatus(t, ctx, "s5", "rook-ceph", "HEALTH_OK", "active+clean", 1, 1, 1)
	doReconcile(t, ctx, r, "s5") // preflight -> advance to ScaleMons

	for i := 0; i < 5; i++ {
		setCephStatus(t, ctx, "s5", "rook-ceph", "HEALTH_ERR", "degraded", 1, 1, 1)
		doReconcile(t, ctx, r, "s5")
	}
	sc = getStorageCluster(t, ctx, "s5")
	if sc.Status.Phase == storagev1alpha1.PhaseHA {
		t.Fatalf("must not reach HA while HEALTH_ERR")
	}
	// Should be parked at an early step, not finalize.
	if migration.Step(sc.Status.MigrationStep) == migration.StepFinalize ||
		migration.Step(sc.Status.MigrationStep) == migration.StepDone {
		t.Fatalf("advanced too far under HEALTH_ERR: %q", sc.Status.MigrationStep)
	}
}
