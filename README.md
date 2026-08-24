# storage-operator

A Kubernetes storage operator that drives [Rook](https://rook.io) to deliver a
zero-config, self-adapting Ceph storage layer.

It does four things:

1. **Autodetects storage disks.** A per-node DaemonSet agent scans for empty raw
   block devices and publishes them as node annotations.
2. **Falls back to Kubernetes-provisioned storage.** When no raw disks are found
   it sources OSD backing storage from an existing `StorageClass`, or from
   operator-managed loopback/raw-file LocalPV — auto-selecting the best option.
3. **Bootstraps Ceph on a single node**, bypassing Rook's multi-node/failure-domain
   defaults (1 mon, `allowMultiplePerNode`, `failureDomain: osd`, replica size 1).
4. **Automatically promotes to HA** once 3+ schedulable nodes are present,
   transparently rebalancing data with no data loss.

## Why an operator (Rook doesn't do this)

Rook has **no** non-HA → HA transition and **no** rebalancing engine of its own.
Rebalancing is native to Ceph (RADOS backfill/recovery + `mgr balancer`), and
"going HA" is a set of individually-reconcilable Rook settings with no ordering
or safety gating. This operator supplies the missing orchestration:

- detects the 1→3 node condition (Rook can't),
- sequences the setting changes in a safe order (Rook won't),
- gates each step on Ceph health / PG state (Rook doesn't),
- throttles the resulting Ceph backfill (Rook doesn't manage this).

## HA promotion state machine

Each step runs once per reconcile and only advances when its health gate passes;
it never forces a change past a failed gate. Replica size only ever *increases*,
so existing data is never removed.

```
Preflight -> ScaleMons(1->3) -> ExpandOSDs -> FlipFailureDomain(osd->host)
          -> RaiseReplicas(1->3) -> WaitRebalance(active+clean) -> Finalize
```

A **stabilization window** (default 300s) requires the node count to stay at the
HA threshold before promotion begins, to avoid flapping. Demotion (3→1) is
deliberately *not* automatic (data-safety); it surfaces as `Degraded`.

## Layout

| Path | Purpose |
|------|---------|
| `api/v1alpha1` | `StorageCluster` CRD types |
| `internal/topology` | node count → desired mode |
| `internal/provisioning` | raw / StorageClass / loopback auto-select |
| `internal/rook` | render + apply Rook Ceph CRDs (unstructured) |
| `internal/migration` | guarded HA promotion state machine |
| `internal/disk` | raw block device detection (lsblk) |
| `internal/controller` | reconcile loop |
| `cmd` | operator manager |
| `cmd/agent` | per-node disk scanner DaemonSet |

## Prerequisites

The Rook Ceph operator must already be installed (this operator drives its CRDs,
it does not fork or bundle Rook).

## Quickstart

```sh
make deploy                       # CRD + RBAC + manager + disk-scanner
kubectl apply -f config/samples/storagecluster.yaml
kubectl get storagecluster -w     # watch Phase: SingleNode -> PromotingToHA -> HA
```

## Development

```sh
make build vet
make test              # unit tests (no cluster)
make test-integration  # envtest control-plane tests (controller + promotion gating)
```

### End-to-end

`make test-e2e` runs the suite (build tag `e2e`) against the current
`KUBECONFIG`; it requires Rook Ceph and this operator installed. The test
creates a `StorageCluster`, waits for Ceph to come up, writes a canary value to
a PVC, and — on 3+ node clusters — asserts promotion to HA (host failure domain,
replica 3) with the canary data surviving the rebalance (no data loss).

To provision everything from scratch on a throwaway 3-node kind cluster:

```sh
make e2e-kind          # creates kind cluster, installs Rook, deploys operator, runs e2e
```

Integration tests use `envtest`; point `ENVTEST_ASSETS` at your control-plane
binaries (or install via `setup-envtest`).
