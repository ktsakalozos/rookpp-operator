//go:build e2e

// Package e2e contains end-to-end tests that run against a real Kubernetes
// cluster with Rook Ceph and the storage-operator installed. Run with:
//
//	go test -tags e2e ./test/e2e/... -timeout 40m
//
// The test uses the current KUBECONFIG context. It is skipped automatically if
// the cluster is unreachable.
package e2e

import (
	"context"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/jackal/storage-operator/api/v1alpha1"
	"github.com/jackal/storage-operator/internal/rook"
)

func e2eClient(t *testing.T) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(storagev1alpha1.AddToScheme(scheme))

	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	)
	cfg, err := loader.ClientConfig()
	if err != nil {
		t.Skipf("no kubeconfig: %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Skipf("cluster unreachable: %v", err)
	}
	return c
}

const (
	scName    = "e2e"
	cephNS    = "rook-ceph"
	appNS     = "default"
	writePod  = "e2e-writer"
	pvcName   = "e2e-data"
	knownData = "storage-operator-e2e-canary-42"
)

// TestEndToEndPromotionPreservesData drives the full lifecycle:
//  1. create a StorageCluster
//  2. wait for Ceph to come up (SingleNode or HA depending on node count)
//  3. provision a PVC from the Rook block StorageClass and write canary data
//  4. if the cluster promotes to HA, verify the canary data survives
func TestEndToEndPromotionPreservesData(t *testing.T) {
	ctx := context.Background()
	c := e2eClient(t)

	// 1. Create the StorageCluster (auto provisioning, auto promote).
	sc := &storagev1alpha1.StorageCluster{}
	sc.Name = scName
	sc.Spec.CephNamespace = cephNS
	sc.Spec.Pools.Block = true
	sc.Spec.HA.AutoPromote = true
	sc.Spec.HA.StabilizationWindowSeconds = 30
	if err := c.Create(ctx, sc); err != nil && !apiExists(err) {
		t.Fatalf("create StorageCluster: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(ctx, sc) })

	// 2. Wait for the operator to report Ceph healthy in some mode.
	waitFor(t, 15*time.Minute, func() bool {
		cur := &storagev1alpha1.StorageCluster{}
		if err := c.Get(ctx, client.ObjectKey{Name: scName}, cur); err != nil {
			return false
		}
		return (cur.Status.Phase == storagev1alpha1.PhaseSingleNode ||
			cur.Status.Phase == storagev1alpha1.PhasePromoting ||
			cur.Status.Phase == storagev1alpha1.PhaseHA) &&
			cur.Status.CephHealth != "HEALTH_ERR" &&
			cur.Status.CephHealth != ""
	})

	// 3. Write canary data through a PVC backed by the block pool.
	writeCanary(t, ctx, c)
	sumBefore := readCanary(t, ctx, c)
	if sumBefore != knownData {
		t.Fatalf("canary mismatch before promotion: %q", sumBefore)
	}

	// 4. If we have >=3 nodes, require the cluster to reach HA and re-verify data.
	nodes := &corev1.NodeList{}
	if err := c.List(ctx, nodes); err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	if countReady(nodes.Items) >= 3 {
		waitFor(t, 25*time.Minute, func() bool {
			cur := &storagev1alpha1.StorageCluster{}
			if err := c.Get(ctx, client.ObjectKey{Name: scName}, cur); err != nil {
				return false
			}
			return cur.Status.Phase == storagev1alpha1.PhaseHA && cur.Status.DataProtected
		})

		// Verify the block pool ended up HA (host failure domain, replica 3).
		pool := &unstructured.Unstructured{}
		pool.SetGroupVersionKind(rook.CephBlockPoolGVK)
		if err := c.Get(ctx, client.ObjectKey{Name: scName + "-block", Namespace: cephNS}, pool); err != nil {
			t.Fatalf("get pool: %v", err)
		}
		fd, _, _ := unstructured.NestedString(pool.Object, "spec", "failureDomain")
		size, _, _ := unstructured.NestedInt64(pool.Object, "spec", "replicated", "size")
		if fd != "host" || size < 3 {
			t.Fatalf("pool not HA after promotion: failureDomain=%q size=%d", fd, size)
		}

		// The canary data must survive the rebalance with no loss.
		sumAfter := readCanary(t, ctx, c)
		if sumAfter != knownData {
			t.Fatalf("DATA LOSS: canary mismatch after promotion: %q", sumAfter)
		}
	} else {
		t.Logf("cluster has <3 ready nodes; skipping HA promotion assertions")
	}
}

func countReady(nodes []corev1.Node) int {
	n := 0
	for i := range nodes {
		for _, cond := range nodes[i].Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
				n++
			}
		}
	}
	return n
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Second)
	}
	t.Fatalf("condition not met within %s", timeout)
}

func apiExists(err error) bool {
	return err != nil && (containsIgnoreCase(err.Error(), "already exists"))
}

func containsIgnoreCase(s, sub string) bool {
	return len(s) >= len(sub) && (indexIgnoreCase(s, sub) >= 0)
}

func indexIgnoreCase(s, sub string) int {
	lower := func(b byte) byte {
		if b >= 'A' && b <= 'Z' {
			return b + 32
		}
		return b
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		ok := true
		for j := 0; j < len(sub); j++ {
			if lower(s[i+j]) != lower(sub[j]) {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}

var _ = os.Getenv
