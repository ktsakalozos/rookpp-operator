package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProvisioningMode selects how backing storage for Ceph OSDs is sourced.
// +kubebuilder:validation:Enum=auto;rawDisksOnly;storageClass;loopback
type ProvisioningMode string

const (
	// ProvisioningAuto tries raw disks, then an existing StorageClass, then loopback files.
	ProvisioningAuto ProvisioningMode = "auto"
	// ProvisioningRawDisksOnly only uses raw block devices.
	ProvisioningRawDisksOnly ProvisioningMode = "rawDisksOnly"
	// ProvisioningStorageClass only uses PVC-based OSDs from an existing StorageClass.
	ProvisioningStorageClass ProvisioningMode = "storageClass"
	// ProvisioningLoopback only uses loopback/raw-file LocalPV.
	ProvisioningLoopback ProvisioningMode = "loopback"
)

// Phase describes the high-level state of the storage cluster.
type Phase string

const (
	PhaseDetecting  Phase = "Detecting"
	PhaseSingleNode Phase = "SingleNode"
	PhasePromoting  Phase = "PromotingToHA"
	PhaseHA         Phase = "HA"
	PhaseDegraded   Phase = "Degraded"
)

// LoopbackSpec configures raw-file backed LocalPV provisioning.
type LoopbackSpec struct {
	// HostPath is the node directory where raw backing files are created.
	// +kubebuilder:default="/var/lib/storage-operator"
	HostPath string `json:"hostPath,omitempty"`
	// SizePerOSD is the size of each loopback backing file (e.g. "50Gi").
	// +kubebuilder:default="50Gi"
	SizePerOSD string `json:"sizePerOSD,omitempty"`
}

// ProvisioningSpec controls where OSD backing storage comes from.
type ProvisioningSpec struct {
	// +kubebuilder:default=auto
	Mode ProvisioningMode `json:"mode,omitempty"`
	// StorageClassName is the preferred StorageClass for PVC-based OSDs.
	StorageClassName string `json:"storageClassName,omitempty"`
	// DiskFilter is a regex applied to device paths during autodetection.
	DiskFilter string `json:"diskFilter,omitempty"`
	// MinDiskSize is the minimum raw device size to consider (e.g. "10Gi").
	MinDiskSize string `json:"minDiskSize,omitempty"`
	// Loopback configures the raw-file fallback.
	Loopback LoopbackSpec `json:"loopback,omitempty"`
}

// HASpec controls single-node to multi-node HA promotion behaviour.
type HASpec struct {
	// AutoPromote enables automatic promotion to HA once enough nodes exist.
	// +kubebuilder:default=true
	AutoPromote bool `json:"autoPromote,omitempty"`
	// MinNodesForHA is the node count that triggers promotion.
	// +kubebuilder:default=3
	MinNodesForHA int `json:"minNodesForHA,omitempty"`
	// TargetReplicas is the pool replica size in HA mode.
	// +kubebuilder:default=3
	TargetReplicas int `json:"targetReplicas,omitempty"`
	// StabilizationWindowSeconds is how long node count must remain >= MinNodesForHA
	// before promotion begins, to avoid flapping.
	// +kubebuilder:default=300
	StabilizationWindowSeconds int `json:"stabilizationWindowSeconds,omitempty"`
	// RebalanceThrottle throttles Ceph backfill during promotion to protect client I/O.
	// +kubebuilder:default=true
	RebalanceThrottle bool `json:"rebalanceThrottle,omitempty"`
}

// PoolsSpec toggles which Ceph consumables are created.
type PoolsSpec struct {
	// +kubebuilder:default=true
	Block bool `json:"block,omitempty"`
	// +kubebuilder:default=false
	Filesystem bool `json:"filesystem,omitempty"`
	// +kubebuilder:default=false
	ObjectStore bool `json:"objectStore,omitempty"`
}

// StorageClusterSpec defines the desired state of StorageCluster.
type StorageClusterSpec struct {
	// Namespace where the Rook CephCluster and its resources live.
	// +kubebuilder:default="rook-ceph"
	CephNamespace string `json:"cephNamespace,omitempty"`
	// CephImage is the Ceph container image passed to Rook.
	// +kubebuilder:default="quay.io/ceph/ceph:v18.2.4"
	CephImage string `json:"cephImage,omitempty"`
	// Provisioning controls OSD backing storage selection.
	Provisioning ProvisioningSpec `json:"provisioning,omitempty"`
	// HA controls single-node to HA promotion.
	HA HASpec `json:"ha,omitempty"`
	// Pools selects which Ceph pools/consumables to create.
	Pools PoolsSpec `json:"pools,omitempty"`
	// ForceSingleNode allows Ceph on a single node bypassing multi-node defaults.
	// +kubebuilder:default=true
	ForceSingleNode bool `json:"forceSingleNode,omitempty"`
}

// DetectedDisk records a raw device found on a node.
type DetectedDisk struct {
	Node       string `json:"node"`
	Path       string `json:"path"`
	SizeBytes  int64  `json:"sizeBytes"`
	Rotational bool   `json:"rotational"`
	Model      string `json:"model,omitempty"`
}

// StorageClusterStatus defines the observed state of StorageCluster.
type StorageClusterStatus struct {
	Phase              Phase          `json:"phase,omitempty"`
	NodeCount          int            `json:"nodeCount,omitempty"`
	ReadyNodeCount     int            `json:"readyNodeCount,omitempty"`
	ProvisioningSource string         `json:"provisioningSource,omitempty"`
	DetectedDisks      []DetectedDisk `json:"detectedDisks,omitempty"`
	CephHealth         string         `json:"cephHealth,omitempty"`
	DataProtected      bool           `json:"dataProtected,omitempty"`
	CurrentReplicas    int            `json:"currentReplicas,omitempty"`
	MigrationStep      string         `json:"migrationStep,omitempty"`
	// HAEligibleSince records when the cluster first became HA-eligible (for the stabilization window).
	HAEligibleSince *metav1.Time       `json:"haEligibleSince,omitempty"`
	Conditions      []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=sc
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Nodes",type=integer,JSONPath=`.status.readyNodeCount`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.status.currentReplicas`
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.status.provisioningSource`
// +kubebuilder:printcolumn:name="Health",type=string,JSONPath=`.status.cephHealth`

// StorageCluster is the Schema for the storageclusters API.
type StorageCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StorageClusterSpec   `json:"spec,omitempty"`
	Status StorageClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StorageClusterList contains a list of StorageCluster.
type StorageClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StorageCluster `json:"items"`
}
