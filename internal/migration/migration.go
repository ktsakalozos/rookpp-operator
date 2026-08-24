// Package migration implements the guarded state machine that promotes a
// single-node Ceph deployment to a highly-available multi-node configuration
// using Rook's individually-reconcilable settings. Rook itself has no
// non-HA -> HA transition; this package supplies the missing orchestration,
// ordering, and health gating.
package migration

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/jackal/rookpp/api/v1alpha1"
	"github.com/jackal/rookpp/internal/rook"
)

// Step is a stage in the promotion process.
type Step string

const (
	StepPreflight     Step = "Preflight"
	StepScaleMons     Step = "ScaleMons"
	StepExpandOSDs    Step = "ExpandOSDs"
	StepFailureDomain Step = "FlipFailureDomain"
	StepRaiseReplicas Step = "RaiseReplicas"
	StepRebalance     Step = "WaitRebalance"
	StepFinalize      Step = "Finalize"
	StepDone          Step = "Done"
)

// order defines the strict progression of promotion steps.
var order = []Step{
	StepPreflight,
	StepScaleMons,
	StepExpandOSDs,
	StepFailureDomain,
	StepRaiseReplicas,
	StepRebalance,
	StepFinalize,
	StepDone,
}

// Next returns the step following s, or StepDone if s is the last/unknown.
func Next(s Step) Step {
	for i, st := range order {
		if st == s && i+1 < len(order) {
			return order[i+1]
		}
	}
	return StepDone
}

// CephStatus is the minimal Ceph state the gate needs.
type CephStatus struct {
	Health         string // HEALTH_OK / HEALTH_WARN / HEALTH_ERR
	MonCount       int
	OSDsUp         int
	OSDsIn         int
	PGsActiveClean bool
	MisplacedRatio float64
	DegradedRatio  float64
}

// StatusReader fetches live Ceph status (from Rook CephCluster status and/or mgr).
type StatusReader interface {
	Read(ctx context.Context, namespace, clusterName string) (CephStatus, error)
}

// Engine drives the promotion state machine. Each Step call performs at most one
// action and returns whether the operator may advance to the next step. It never
// forces a change past a failed health gate; instead it returns advance=false so
// the reconcile loop requeues.
type Engine struct {
	Client     client.Client
	Status     StatusReader
	FieldOwner string
}

// Result is the outcome of a single step execution.
type Result struct {
	Advance bool
	Message string
}

// Execute runs the given step for the StorageCluster and reports whether the
// state machine may advance. Steps are idempotent and safe to re-run.
func (e *Engine) Execute(ctx context.Context, sc *storagev1alpha1.StorageCluster, step Step, want rook.ClusterParams) (Result, error) {
	cs, err := e.Status.Read(ctx, sc.Spec.CephNamespace, sc.Name)
	if err != nil {
		return Result{Advance: false, Message: "unable to read ceph status: " + err.Error()}, nil
	}

	switch step {
	case StepPreflight:
		return e.preflight(sc, cs)
	case StepScaleMons:
		return e.scaleMons(ctx, sc, want, cs)
	case StepExpandOSDs:
		return e.expandOSDs(ctx, sc, want, cs)
	case StepFailureDomain:
		return e.flipFailureDomain(ctx, sc, want, cs)
	case StepRaiseReplicas:
		return e.raiseReplicas(ctx, sc, want, cs)
	case StepRebalance:
		return e.waitRebalance(cs)
	case StepFinalize:
		return Result{Advance: true, Message: "promotion complete"}, nil
	case StepDone:
		return Result{Advance: false, Message: "already HA"}, nil
	default:
		return Result{}, fmt.Errorf("unknown step %q", step)
	}
}

// healthy reports whether Ceph is in an acceptable state to make a change.
// We tolerate HEALTH_WARN (common during backfill) but never HEALTH_ERR.
func healthy(cs CephStatus) bool {
	return cs.Health == "HEALTH_OK" || cs.Health == "HEALTH_WARN"
}

func (e *Engine) preflight(sc *storagev1alpha1.StorageCluster, cs CephStatus) (Result, error) {
	if cs.Health == "HEALTH_ERR" {
		return Result{Advance: false, Message: "refusing promotion: cluster HEALTH_ERR"}, nil
	}
	if !cs.PGsActiveClean {
		return Result{Advance: false, Message: "waiting for PGs active+clean before promotion"}, nil
	}
	return Result{Advance: true, Message: "preflight ok"}, nil
}

func (e *Engine) scaleMons(ctx context.Context, sc *storagev1alpha1.StorageCluster, want rook.ClusterParams, cs CephStatus) (Result, error) {
	if !healthy(cs) {
		return Result{Advance: false, Message: "unhealthy; hold mon scale"}, nil
	}
	want.MonCount = 3
	want.MonAllowMultiplePer = false
	if err := e.applyCluster(ctx, sc, want); err != nil {
		return Result{}, err
	}
	if cs.MonCount < 3 {
		return Result{Advance: false, Message: "waiting for mon quorum (3)"}, nil
	}
	return Result{Advance: true, Message: "mons scaled to 3"}, nil
}

func (e *Engine) expandOSDs(ctx context.Context, sc *storagev1alpha1.StorageCluster, want rook.ClusterParams, cs CephStatus) (Result, error) {
	if !healthy(cs) {
		return Result{Advance: false, Message: "unhealthy; hold OSD expansion"}, nil
	}
	// The desired params already carry the (re-evaluated) storage source for all
	// schedulable nodes; applying ensures OSDs come up on the new hosts.
	if err := e.applyCluster(ctx, sc, want); err != nil {
		return Result{}, err
	}
	if cs.OSDsUp < 3 || cs.OSDsIn < 3 {
		return Result{Advance: false, Message: fmt.Sprintf("waiting for OSDs across hosts (up=%d in=%d)", cs.OSDsUp, cs.OSDsIn)}, nil
	}
	return Result{Advance: true, Message: "OSDs present on 3+ hosts"}, nil
}

// flipFailureDomain moves the pool CRUSH rule from osd -> host. This is the
// riskiest change on a populated pool; it is isolated in its own gated step.
func (e *Engine) flipFailureDomain(ctx context.Context, sc *storagev1alpha1.StorageCluster, want rook.ClusterParams, cs CephStatus) (Result, error) {
	if !healthy(cs) {
		return Result{Advance: false, Message: "unhealthy; hold failure-domain flip"}, nil
	}
	pool := rook.BuildBlockPool(rook.PoolName(sc), sc.Spec.CephNamespace, "host", currentReplicas(sc), false)
	if err := rook.Apply(ctx, e.Client, pool, e.FieldOwner); err != nil {
		return Result{}, err
	}
	if !cs.PGsActiveClean {
		return Result{Advance: false, Message: "failure domain flipped; waiting for PGs active+clean"}, nil
	}
	return Result{Advance: true, Message: "failure domain now host"}, nil
}

// raiseReplicas increases replica size to the target. Increasing size only adds
// copies; it never deletes the existing data. requireSafeReplicaSize is enabled.
func (e *Engine) raiseReplicas(ctx context.Context, sc *storagev1alpha1.StorageCluster, want rook.ClusterParams, cs CephStatus) (Result, error) {
	if !healthy(cs) {
		return Result{Advance: false, Message: "unhealthy; hold replica raise"}, nil
	}
	target := sc.Spec.HA.TargetReplicas
	if target < 1 {
		target = 3
	}
	pool := rook.BuildBlockPool(rook.PoolName(sc), sc.Spec.CephNamespace, "host", target, true)
	if err := rook.Apply(ctx, e.Client, pool, e.FieldOwner); err != nil {
		return Result{}, err
	}
	return Result{Advance: true, Message: fmt.Sprintf("replica size set to %d", target)}, nil
}

// waitRebalance blocks until Ceph's recovery/backfill has converged.
func (e *Engine) waitRebalance(cs CephStatus) (Result, error) {
	if cs.Health == "HEALTH_ERR" {
		return Result{Advance: false, Message: "rebalance stalled: HEALTH_ERR"}, nil
	}
	if !cs.PGsActiveClean || cs.MisplacedRatio > 0.001 || cs.DegradedRatio > 0 {
		return Result{Advance: false, Message: fmt.Sprintf("rebalancing (misplaced=%.3f degraded=%.3f)", cs.MisplacedRatio, cs.DegradedRatio)}, nil
	}
	return Result{Advance: true, Message: "rebalance complete; data fully replicated"}, nil
}

func (e *Engine) applyCluster(ctx context.Context, sc *storagev1alpha1.StorageCluster, want rook.ClusterParams) error {
	u := rook.BuildCephCluster(want)
	return rook.Apply(ctx, e.Client, u, e.FieldOwner)
}

func currentReplicas(sc *storagev1alpha1.StorageCluster) int {
	if sc.Status.CurrentReplicas > 0 {
		return sc.Status.CurrentReplicas
	}
	return 1
}

var _ = unstructured.Unstructured{}
