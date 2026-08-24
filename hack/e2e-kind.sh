#!/usr/bin/env bash
# Bootstraps a 3-node kind cluster, installs Rook Ceph, builds+loads the
# rookpp images, deploys the operator, and runs the e2e suite.
#
# Requirements: kind, kubectl, docker, helm, go.
set -euo pipefail

CLUSTER="${CLUSTER:-rookpp-e2e}"
IMG_MANAGER="${IMG_MANAGER:-ghcr.io/jackal/rookpp:e2e}"
IMG_AGENT="${IMG_AGENT:-ghcr.io/jackal/rookpp-agent:e2e}"
ROOK_VERSION="${ROOK_VERSION:-v1.15.0}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

echo ">> creating kind cluster ${CLUSTER} (3 nodes)"
cat <<EOF | kind create cluster --name "${CLUSTER}" --config -
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
  - role: worker
  - role: worker
EOF

echo ">> building images"
docker build --target manager -t "${IMG_MANAGER}" "${ROOT}"
docker build --target agent   -t "${IMG_AGENT}"   "${ROOT}"
kind load docker-image "${IMG_MANAGER}" --name "${CLUSTER}"
kind load docker-image "${IMG_AGENT}"   --name "${CLUSTER}"

echo ">> installing Rook Ceph operator ${ROOK_VERSION}"
helm repo add rook-release https://charts.rook.io/release >/dev/null 2>&1 || true
helm repo update >/dev/null
helm upgrade --install rook-ceph rook-release/rook-ceph \
  --version "${ROOK_VERSION}" \
  --namespace rook-ceph --create-namespace --wait

echo ">> deploying rookpp"
kubectl apply -f "${ROOT}/config/crd/storagecluster.yaml"
kubectl apply -f "${ROOT}/config/rbac/role.yaml"
sed -e "s#ghcr.io/jackal/rookpp:latest#${IMG_MANAGER}#" \
    -e "s#ghcr.io/jackal/rookpp-agent:latest#${IMG_AGENT}#" \
    "${ROOT}/config/manager/manager.yaml" | kubectl apply -f -

kubectl -n rookpp-system rollout status deploy/rookpp-controller --timeout=180s

echo ">> defining a block StorageClass for canary PVCs (E2E_STORAGECLASS=ceph-block)"
# The operator creates the CephBlockPool; a consumer StorageClass is defined here.
cat <<'EOF' | kubectl apply -f -
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: ceph-block
provisioner: rook-ceph.rbd.csi.ceph.com
parameters:
  clusterID: rook-ceph
  pool: e2e-block
  imageFormat: "2"
  imageFeatures: layering
  csi.storage.k8s.io/fstype: ext4
allowVolumeExpansion: true
reclaimPolicy: Delete
EOF

echo ">> running e2e suite"
cd "${ROOT}"
E2E_STORAGECLASS=ceph-block go test -tags e2e ./test/e2e/... -timeout 40m -v

echo ">> done. delete with: kind delete cluster --name ${CLUSTER}"
