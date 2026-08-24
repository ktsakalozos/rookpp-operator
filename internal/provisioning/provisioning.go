// Package provisioning selects and prepares the backing storage source for Ceph OSDs.
package provisioning

import (
	"context"
	"fmt"

	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/jackal/rookpp/api/v1alpha1"
	"github.com/jackal/rookpp/internal/rook"
)

// Source identifies the chosen backing storage strategy.
type Source string

const (
	SourceRawDisks     Source = "RawDisks"
	SourceStorageClass Source = "StorageClass"
	SourceLoopback     Source = "Loopback"
	SourceNone         Source = "None"
)

// Decision is the outcome of Select: which source and how to wire it into Rook.
type Decision struct {
	Source           Source
	DeviceFilter     string
	UseAllDevices    bool
	StorageClassName string
	OSDCount         int
	OSDSize          string
	Reason           string
}

// Manager selects the provisioning source based on spec and detected disks.
type Manager struct {
	Client client.Client
}

// LoopbackStorageClassName is the SC created by the operator's loopback provisioner.
const LoopbackStorageClassName = "rookpp-loopback"

// Select applies the auto-selection order:
//  1. raw disks, 2. usable existing StorageClass, 3. loopback fallback.
func (m *Manager) Select(ctx context.Context, sc *storagev1alpha1.StorageCluster, disks []storagev1alpha1.DetectedDisk) (Decision, error) {
	mode := sc.Spec.Provisioning.Mode
	if mode == "" {
		mode = storagev1alpha1.ProvisioningAuto
	}

	switch mode {
	case storagev1alpha1.ProvisioningRawDisksOnly:
		if len(disks) == 0 {
			return Decision{Source: SourceNone, Reason: "rawDisksOnly but no raw disks detected"}, nil
		}
		return m.rawDecision(sc), nil
	case storagev1alpha1.ProvisioningStorageClass:
		return m.storageClassDecision(ctx, sc, len(disks))
	case storagev1alpha1.ProvisioningLoopback:
		return m.loopbackDecision(sc), nil
	case storagev1alpha1.ProvisioningAuto:
		// 1. Raw disks preferred.
		if len(disks) > 0 {
			return m.rawDecision(sc), nil
		}
		// 2. Existing usable StorageClass.
		if d, ok, err := m.tryStorageClass(ctx, sc); err != nil {
			return Decision{}, err
		} else if ok {
			return d, nil
		}
		// 3. Loopback fallback.
		return m.loopbackDecision(sc), nil
	default:
		return Decision{}, fmt.Errorf("unknown provisioning mode %q", mode)
	}
}

func (m *Manager) rawDecision(sc *storagev1alpha1.StorageCluster) Decision {
	filter := sc.Spec.Provisioning.DiskFilter
	return Decision{
		Source:        SourceRawDisks,
		UseAllDevices: filter == "",
		DeviceFilter:  filter,
		Reason:        "raw block devices detected",
	}
}

func (m *Manager) storageClassDecision(ctx context.Context, sc *storagev1alpha1.StorageCluster, diskCount int) (Decision, error) {
	d, ok, err := m.tryStorageClass(ctx, sc)
	if err != nil {
		return Decision{}, err
	}
	if !ok {
		return Decision{Source: SourceNone, Reason: "storageClass mode but no usable StorageClass found"}, nil
	}
	return d, nil
}

// tryStorageClass returns a StorageClass-backed decision if a usable SC exists.
func (m *Manager) tryStorageClass(ctx context.Context, sc *storagev1alpha1.StorageCluster) (Decision, bool, error) {
	preferred := sc.Spec.Provisioning.StorageClassName
	scList := &storagev1.StorageClassList{}
	if err := m.Client.List(ctx, scList); err != nil {
		return Decision{}, false, fmt.Errorf("list storageclasses: %w", err)
	}

	var chosen string
	for i := range scList.Items {
		item := &scList.Items[i]
		// Never consume our own loopback SC here; it is handled by loopbackDecision.
		if item.Name == LoopbackStorageClassName {
			continue
		}
		if preferred != "" {
			if item.Name == preferred {
				chosen = item.Name
				break
			}
			continue
		}
		// No preference: pick the default SC if present.
		if isDefaultSC(item) {
			chosen = item.Name
			break
		}
	}
	if chosen == "" {
		return Decision{}, false, nil
	}
	return Decision{
		Source:           SourceStorageClass,
		StorageClassName: chosen,
		OSDCount:         osdCountForMode(sc),
		OSDSize:          sc.Spec.Provisioning.Loopback.SizePerOSD,
		Reason:           "using existing StorageClass " + chosen,
	}, true, nil
}

func (m *Manager) loopbackDecision(sc *storagev1alpha1.StorageCluster) Decision {
	size := sc.Spec.Provisioning.Loopback.SizePerOSD
	if size == "" {
		size = "50Gi"
	}
	return Decision{
		Source:           SourceLoopback,
		StorageClassName: LoopbackStorageClassName,
		OSDCount:         osdCountForMode(sc),
		OSDSize:          size,
		Reason:           "no raw disks or StorageClass; using loopback LocalPV",
	}
}

// osdCountForMode returns a sensible OSD count: enough to satisfy replication.
func osdCountForMode(sc *storagev1alpha1.StorageCluster) int {
	replicas := sc.Spec.HA.TargetReplicas
	if replicas < 1 {
		replicas = 3
	}
	return replicas
}

// ToClusterParams merges a Decision into rook.ClusterParams storage fields.
func (d Decision) ToClusterParams(p *rook.ClusterParams) error {
	switch d.Source {
	case SourceRawDisks:
		p.UseAllDevices = d.UseAllDevices
		p.DeviceFilter = d.DeviceFilter
		p.StorageClassDevices = nil
	case SourceStorageClass, SourceLoopback:
		size := d.OSDSize
		if size == "" {
			size = "50Gi"
		}
		if _, err := resource.ParseQuantity(size); err != nil {
			return fmt.Errorf("invalid OSD size %q: %w", size, err)
		}
		p.StorageClassDevices = &rook.StorageClassDeviceSet{
			Name:             "osd-set",
			Count:            d.OSDCount,
			StorageClassName: d.StorageClassName,
			Size:             size,
		}
	case SourceNone:
		return fmt.Errorf("no provisioning source available: %s", d.Reason)
	}
	return nil
}

func isDefaultSC(sc *storagev1.StorageClass) bool {
	if sc.Annotations == nil {
		return false
	}
	return sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true"
}

var _ = metav1.ObjectMeta{}
