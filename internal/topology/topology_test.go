package topology

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func node(name string, ready bool, unschedulable bool, taint string) corev1.Node {
	n := corev1.Node{}
	n.Name = name
	cond := corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionFalse}
	if ready {
		cond.Status = corev1.ConditionTrue
	}
	n.Status.Conditions = []corev1.NodeCondition{cond}
	n.Spec.Unschedulable = unschedulable
	if taint != "" {
		n.Spec.Taints = []corev1.Taint{{Key: taint, Effect: corev1.TaintEffectNoSchedule}}
	}
	return n
}

func TestAnalyzeAndDesiredMode(t *testing.T) {
	nodes := []corev1.Node{
		node("a", true, false, ""),
		node("b", true, false, ""),
		node("c", true, false, ""),
		node("cp", true, false, "node-role.kubernetes.io/control-plane"),
		node("down", false, false, ""),
	}
	s := Analyze(nodes)
	if s.SchedulableStorageNodes != 3 {
		t.Fatalf("schedulable storage nodes = %d want 3", s.SchedulableStorageNodes)
	}
	if s.DesiredMode(3) != ModeHA {
		t.Fatalf("expected HA mode at 3 storage nodes")
	}
	if s.DesiredMode(4) != ModeSingleNode {
		t.Fatalf("expected SingleNode below threshold")
	}
}

func TestSingleNode(t *testing.T) {
	s := Analyze([]corev1.Node{node("only", true, false, "")})
	if s.DesiredMode(3) != ModeSingleNode {
		t.Fatalf("single node cluster should be SingleNode mode")
	}
}
