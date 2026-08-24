package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/jackal/rookpp/internal/migration"
	"github.com/jackal/rookpp/internal/rook"
)

// cephStatusReader reads Ceph status from the Rook CephCluster status
// subresource. It implements migration.StatusReader. Kept in the controller
// package to avoid a rook<->migration import cycle.
type cephStatusReader struct {
	Client client.Client
}

func (r cephStatusReader) Read(ctx context.Context, namespace, clusterName string) (migration.CephStatus, error) {
	u, err := rook.GetCephCluster(ctx, r.Client, clusterName, namespace)
	if err != nil {
		return migration.CephStatus{}, err
	}
	return translateCephStatus(u), nil
}

func translateCephStatus(u *unstructured.Unstructured) migration.CephStatus {
	cs := migration.CephStatus{Health: rook.ReadCephHealth(u)}

	if mons, found, _ := unstructured.NestedMap(u.Object, "status", "ceph", "mons"); found {
		cs.MonCount = len(mons)
	}
	if v, found, _ := unstructured.NestedInt64(u.Object, "status", "ceph", "osd", "up"); found {
		cs.OSDsUp = int(v)
	}
	if v, found, _ := unstructured.NestedInt64(u.Object, "status", "ceph", "osd", "in"); found {
		cs.OSDsIn = int(v)
	}
	pgState, _, _ := unstructured.NestedString(u.Object, "status", "ceph", "pgState")
	cs.PGsActiveClean = pgState == "" || pgState == "active+clean"

	return cs
}
