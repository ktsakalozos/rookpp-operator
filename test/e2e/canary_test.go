//go:build e2e

package e2e

import (
	"context"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// storageClass returns the StorageClass used to provision the canary PVC.
// Rook exposes block storage via a StorageClass the admin defines against the
// CephBlockPool; override with E2E_STORAGECLASS.
func storageClass() string {
	if v := os.Getenv("E2E_STORAGECLASS"); v != "" {
		return v
	}
	return "ceph-block"
}

func clientset(t *testing.T) *kubernetes.Clientset {
	t.Helper()
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	)
	cfg, err := loader.ClientConfig()
	if err != nil {
		t.Skipf("no kubeconfig: %v", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}
	return cs
}

func ensurePVC(t *testing.T, ctx context.Context, c client.Client) {
	t.Helper()
	sc := storageClass()
	q := resource.MustParse("1Gi")
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: pvcName, Namespace: appNS},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &sc,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: q},
			},
		},
	}
	if err := c.Create(ctx, pvc); err != nil && !apiExists(err) {
		t.Fatalf("create pvc: %v", err)
	}
}

// writeCanary runs a short-lived pod that writes the known canary string into
// the PVC, then waits for it to complete.
func writeCanary(t *testing.T, ctx context.Context, c client.Client) {
	t.Helper()
	ensurePVC(t, ctx, c)
	runCanaryPod(t, ctx, c, writePod,
		[]string{"sh", "-c", "echo -n " + knownData + " > /data/canary && sync"})
}

// readCanary runs a pod that prints the canary file; its logs are the result.
func readCanary(t *testing.T, ctx context.Context, c client.Client) string {
	t.Helper()
	const readerPod = "e2e-reader"
	runCanaryPod(t, ctx, c, readerPod, []string{"sh", "-c", "cat /data/canary"})
	cs := clientset(t)
	logs, err := cs.CoreV1().Pods(appNS).GetLogs(readerPod, &corev1.PodLogOptions{}).DoRaw(ctx)
	if err != nil {
		t.Fatalf("read pod logs: %v", err)
	}
	return string(logs)
}

func runCanaryPod(t *testing.T, ctx context.Context, c client.Client, name string, cmd []string) {
	t.Helper()
	_ = c.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appNS}})
	// Wait for prior pod to be gone.
	waitFor(t, 2*time.Minute, func() bool {
		p := &corev1.Pod{}
		err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: appNS}, p)
		return err != nil
	})

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appNS},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:    "canary",
				Image:   "busybox:1.36",
				Command: cmd,
				VolumeMounts: []corev1.VolumeMount{{
					Name:      "data",
					MountPath: "/data",
				}},
			}},
			Volumes: []corev1.Volume{{
				Name: "data",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: pvcName,
					},
				},
			}},
		},
	}
	if err := c.Create(ctx, pod); err != nil {
		t.Fatalf("create pod %s: %v", name, err)
	}
	// Wait for completion.
	waitFor(t, 5*time.Minute, func() bool {
		p := &corev1.Pod{}
		if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: appNS}, p); err != nil {
			return false
		}
		return p.Status.Phase == corev1.PodSucceeded
	})
}
