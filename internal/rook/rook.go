// Package rook renders and applies Rook Ceph CRDs as unstructured objects so the
// operator can drive Rook without a compile-time dependency on the Rook API module.
package rook

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/jackal/rookpp/api/v1alpha1"
)

// GVKs for the Rook CRDs we manage.
var (
	CephClusterGVK   = schema.GroupVersionKind{Group: "ceph.rook.io", Version: "v1", Kind: "CephCluster"}
	CephBlockPoolGVK = schema.GroupVersionKind{Group: "ceph.rook.io", Version: "v1", Kind: "CephBlockPool"}
)

// ClusterParams captures the tunables that differ between single-node and HA.
type ClusterParams struct {
	Name      string
	Namespace string
	CephImage string

	MonCount            int
	MonAllowMultiplePer bool
	MgrCount            int
	FailureDomain       string // "osd" (single-node) or "host" (HA)
	ReplicaSize         int
	RequireSafeReplica  bool

	// Storage source. Exactly one of these should be set by the provisioner.
	UseAllDevices       bool
	DeviceFilter        string
	StorageClassDevices *StorageClassDeviceSet
}

// StorageClassDeviceSet describes PVC-based OSDs backed by a StorageClass.
type StorageClassDeviceSet struct {
	Name             string
	Count            int
	StorageClassName string
	Size             string
}

// BuildCephCluster renders a CephCluster unstructured object from params.
func BuildCephCluster(p ClusterParams) *unstructured.Unstructured {
	storage := map[string]interface{}{}
	if p.StorageClassDevices != nil {
		storage["storageClassDeviceSets"] = []interface{}{
			map[string]interface{}{
				"name":                 p.StorageClassDevices.Name,
				"count":                int64(p.StorageClassDevices.Count),
				"portable":             true,
				"tuneDeviceClass":      true,
				"volumeClaimTemplates": scDeviceClaimTemplate(p.StorageClassDevices),
			},
		}
	} else {
		storage["useAllNodes"] = true
		storage["useAllDevices"] = p.UseAllDevices
		if p.DeviceFilter != "" {
			storage["deviceFilter"] = p.DeviceFilter
		}
	}

	obj := map[string]interface{}{
		"spec": map[string]interface{}{
			"cephVersion": map[string]interface{}{
				"image": p.CephImage,
			},
			"dataDirHostPath": "/var/lib/rook",
			"mon": map[string]interface{}{
				"count":                int64(p.MonCount),
				"allowMultiplePerNode": p.MonAllowMultiplePer,
			},
			"mgr": map[string]interface{}{
				"count": int64(p.MgrCount),
			},
			"dashboard": map[string]interface{}{"enabled": false},
			"storage":   storage,
		},
	}

	u := &unstructured.Unstructured{Object: obj}
	u.SetGroupVersionKind(CephClusterGVK)
	u.SetName(p.Name)
	u.SetNamespace(p.Namespace)
	return u
}

func scDeviceClaimTemplate(d *StorageClassDeviceSet) []interface{} {
	return []interface{}{
		map[string]interface{}{
			"metadata": map[string]interface{}{"name": "data"},
			"spec": map[string]interface{}{
				"accessModes":      []interface{}{"ReadWriteOnce"},
				"volumeMode":       "Block",
				"storageClassName": d.StorageClassName,
				"resources": map[string]interface{}{
					"requests": map[string]interface{}{"storage": d.Size},
				},
			},
		},
	}
}

// BuildBlockPool renders a CephBlockPool with the given replication settings.
func BuildBlockPool(name, namespace, failureDomain string, replicas int, requireSafe bool) *unstructured.Unstructured {
	obj := map[string]interface{}{
		"spec": map[string]interface{}{
			"failureDomain": failureDomain,
			"replicated": map[string]interface{}{
				"size":                   int64(replicas),
				"requireSafeReplicaSize": requireSafe,
			},
		},
	}
	u := &unstructured.Unstructured{Object: obj}
	u.SetGroupVersionKind(CephBlockPoolGVK)
	u.SetName(name)
	u.SetNamespace(namespace)
	return u
}

// Apply performs a server-side apply-style upsert of an unstructured object.
func Apply(ctx context.Context, c client.Client, desired *unstructured.Unstructured, fieldOwner string) error {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(desired.GroupVersionKind())
	key := types.NamespacedName{Name: desired.GetName(), Namespace: desired.GetNamespace()}
	err := c.Get(ctx, key, existing)
	if err != nil {
		if client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("get %s: %w", desired.GetKind(), err)
		}
		if err := c.Create(ctx, desired); err != nil {
			return fmt.Errorf("create %s: %w", desired.GetKind(), err)
		}
		return nil
	}
	// Merge desired spec onto existing to preserve server-managed fields.
	desired.SetResourceVersion(existing.GetResourceVersion())
	if err := c.Update(ctx, desired); err != nil {
		return fmt.Errorf("update %s: %w", desired.GetKind(), err)
	}
	return nil
}

// GetCephCluster fetches the current CephCluster, if any.
func GetCephCluster(ctx context.Context, c client.Client, name, namespace string) (*unstructured.Unstructured, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(CephClusterGVK)
	err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, u)
	if err != nil {
		return nil, err
	}
	return u, nil
}

// ReadCephHealth extracts status.ceph.health from a CephCluster object.
func ReadCephHealth(u *unstructured.Unstructured) string {
	if u == nil {
		return ""
	}
	health, found, err := unstructured.NestedString(u.Object, "status", "ceph", "health")
	if err != nil || !found {
		return ""
	}
	return health
}

// PoolName returns the canonical block pool name for a cluster.
func PoolName(sc *storagev1alpha1.StorageCluster) string {
	return sc.Name + "-block"
}
