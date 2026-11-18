#!/usr/bin/env bash
# E2E: kind cluster -> install driver -> PVC write -> snapshot -> clone -> verify data.
# Requires: docker, kind, kubectl, helm. Run: ./hack/e2e-kind.sh
set -euo pipefail
CLUSTER="${CLUSTER:-snapvol}"
IMAGE="snapvol-csi:e2e"

kind create cluster --name "$CLUSTER" 2>/dev/null || true
export KUBECONFIG="$(kind get kubeconfig-path --name "$CLUSTER" 2>/dev/null || kind get kubeconfig --name "$CLUSTER")"
docker build -t "$IMAGE" .
kind load docker-image "$IMAGE" --name "$CLUSTER"
helm upgrade --install snapvol deploy/helm -n kube-system \
  --set image.repository=snapvol-csi --set image.tag=e2e --set image.pullPolicy=Never

kubectl apply -f - <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: src}
spec: {accessModes: [ReadWriteOnce], storageClassName: snapvol, resources: {requests: {storage: 1Gi}}}
---
apiVersion: v1
kind: Pod
metadata: {name: writer}
spec:
  containers: [{name: c, image: busybox, command: ["sh","-c","echo hello-snapvol > /data/f.txt && sleep 3600"], volumeMounts: [{name: v, mountPath: /data}]}]
  volumes: [{name: v, persistentVolumeClaim: {claimName: src}}]
EOF
kubectl wait --for=condition=Ready pod/writer --timeout=180s

kubectl apply -f - <<'EOF'
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata: {name: snap}
spec: {volumeSnapshotClassName: snapvol, source: {persistentVolumeClaimName: src}}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: clone}
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: snapvol
  resources: {requests: {storage: 1Gi}}
  dataSource: {name: snap, kind: VolumeSnapshot, apiGroup: snapshot.storage.k8s.io}
---
apiVersion: v1
kind: Pod
metadata: {name: reader}
spec:
  containers: [{name: c, image: busybox, command: ["sh","-c","cat /data/f.txt && sleep 3600"], volumeMounts: [{name: v, mountPath: /data}]}]
  volumes: [{name: v, persistentVolumeClaim: {claimName: clone}}]
EOF
kubectl wait --for=condition=Ready pod/reader --timeout=180s
kubectl logs reader | grep -q hello-snapvol && echo "E2E PASS: cloned data verified"
