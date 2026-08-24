// Package topology decides the desired Ceph mode from cluster node topology.
package topology

import (
	corev1 "k8s.io/api/core/v1"
)

// Mode is the desired Ceph operating mode.
type Mode string

const (
	// ModeSingleNode runs Ceph on a single node bypassing multi-node defaults.
	ModeSingleNode Mode = "SingleNode"
	// ModeHA runs Ceph with host-level failure domain and replicated pools.
	ModeHA Mode = "HA"
)

// Snapshot summarises the cluster topology relevant to storage decisions.
type Snapshot struct {
	TotalNodes int
	ReadyNodes int
	// SchedulableStorageNodes is the count of Ready nodes that can host OSDs
	// (Ready, not unschedulable, not tainted NoSchedule without our toleration).
	SchedulableStorageNodes int
	ReadyNodeNames          []string
}

// controlPlaneTaints are taints that, when present as NoSchedule, exclude a node
// from hosting OSDs unless explicitly tolerated.
var controlPlaneTaints = map[string]struct{}{
	"node-role.kubernetes.io/control-plane": {},
	"node-role.kubernetes.io/master":        {},
}

// Analyze builds a Snapshot from the node list.
func Analyze(nodes []corev1.Node) Snapshot {
	s := Snapshot{TotalNodes: len(nodes)}
	for i := range nodes {
		n := &nodes[i]
		if !isReady(n) {
			continue
		}
		s.ReadyNodes++
		s.ReadyNodeNames = append(s.ReadyNodeNames, n.Name)
		if n.Spec.Unschedulable {
			continue
		}
		if hasBlockingTaint(n) {
			continue
		}
		s.SchedulableStorageNodes++
	}
	return s
}

// DesiredMode returns the desired mode for the given snapshot and HA threshold.
func (s Snapshot) DesiredMode(minNodesForHA int) Mode {
	if s.SchedulableStorageNodes >= minNodesForHA {
		return ModeHA
	}
	return ModeSingleNode
}

// HAEligible reports whether the topology currently satisfies the HA threshold.
func (s Snapshot) HAEligible(minNodesForHA int) bool {
	return s.SchedulableStorageNodes >= minNodesForHA
}

func isReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func hasBlockingTaint(n *corev1.Node) bool {
	for _, t := range n.Spec.Taints {
		if t.Effect != corev1.TaintEffectNoSchedule && t.Effect != corev1.TaintEffectNoExecute {
			continue
		}
		if _, ok := controlPlaneTaints[t.Key]; ok {
			return true
		}
	}
	return false
}
