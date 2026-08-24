package controller

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	storagev1alpha1 "github.com/jackal/rookpp/api/v1alpha1"
)

var (
	testEnv    *envtest.Environment
	testCfg    *rest.Config
	testK8s    client.Client
	testScheme = runtime.NewScheme()
)

// setup starts an envtest control plane with our CRD + minimal Rook CRDs.
// It returns a teardown func. Tests skip if envtest assets are unavailable.
func setup(t *testing.T) func() {
	t.Helper()

	utilruntime.Must(clientgoscheme.AddToScheme(testScheme))
	utilruntime.Must(storagev1alpha1.AddToScheme(testScheme))

	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd"),
			filepath.Join("testdata", "crds"),
		},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		t.Skipf("skipping envtest (control plane unavailable): %v", err)
	}
	testCfg = cfg

	k8s, err := client.New(cfg, client.Options{Scheme: testScheme})
	if err != nil {
		_ = testEnv.Stop()
		t.Fatalf("create client: %v", err)
	}
	testK8s = k8s

	return func() { _ = testEnv.Stop() }
}

func newReconciler() *StorageClusterReconciler {
	return &StorageClusterReconciler{Client: testK8s, Scheme: testScheme}
}

// --- test helpers -----------------------------------------------------------

func mkNode(t *testing.T, ctx context.Context, name string, ready bool) {
	t.Helper()
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := testK8s.Create(ctx, n); err != nil {
		t.Fatalf("create node %s: %v", name, err)
	}
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: st}}
	if err := testK8s.Status().Update(ctx, n); err != nil {
		t.Fatalf("update node status %s: %v", name, err)
	}
}

func mkDefaultStorageClass(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	sc := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: map[string]string{"storageclass.kubernetes.io/is-default-class": "true"},
		},
		Provisioner: "example.com/fake",
	}
	if err := testK8s.Create(ctx, sc); err != nil {
		t.Fatalf("create storageclass: %v", err)
	}
}

func mkStorageCluster(t *testing.T, ctx context.Context, name string, mutate func(*storagev1alpha1.StorageCluster)) *storagev1alpha1.StorageCluster {
	t.Helper()
	sc := &storagev1alpha1.StorageCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: storagev1alpha1.StorageClusterSpec{
			CephNamespace: "rook-ceph",
			Pools:         storagev1alpha1.PoolsSpec{Block: true},
		},
	}
	if mutate != nil {
		mutate(sc)
	}
	if err := testK8s.Create(ctx, sc); err != nil {
		t.Fatalf("create storagecluster: %v", err)
	}
	return sc
}

func mkNamespace(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	_ = testK8s.Create(ctx, ns) // ignore AlreadyExists
}

func getStorageCluster(t *testing.T, ctx context.Context, name string) *storagev1alpha1.StorageCluster {
	t.Helper()
	sc := &storagev1alpha1.StorageCluster{}
	if err := testK8s.Get(ctx, client.ObjectKey{Name: name}, sc); err != nil {
		t.Fatalf("get storagecluster: %v", err)
	}
	return sc
}

var _ = time.Second
