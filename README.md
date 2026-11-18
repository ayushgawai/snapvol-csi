# snapvol-csi

A Kubernetes CSI driver plus a small cloud-volume control plane in Go, in the
spirit of storage-orchestrator drivers for cloud file services. It provisions
file volumes, snapshots them, and clones volumes from snapshots — against a
local-filesystem backend today, with interfaces stubbed for AWS FSx, Azure
Files, and GCP Filestore.

## Architecture

```
                Kubernetes control plane
        ┌─────────────────────────────────────────────┐
        │  external-provisioner / snapshotter / resizer │
        └───────────────┬─────────────────────────────┘
                        │ CSI gRPC (unix socket)
        ┌───────────────▼──────────────┐   ┌────────────────────────────┐
        │  snapvol controller (Deploy) │   │  snapvol control plane     │
        │  Identity/Controller         │   │  REST API (OpenAPI)        │
        └───────────────┬──────────────┘   │  idempotency · quota ·     │
                        │                  │  async job queue · retries │
                        │  backend.Backend └─────────────┬──────────────┘
        ┌───────────────▼────────────────────────────────▼──────────────┐
        │  local-fs (implemented)   │ FSx │ Azure Files │ GCP Filestore │
        │  volumes = dirs,          │ stub│ stub        │ stub          │
        │  snapshots = copies       │     │             │               │
        └───────────────▲───────────────────────────────────────────────┘
                        │ path / NFS export
        ┌───────────────┴──────────────┐
        │  snapvol node (DaemonSet)    │  NodeStage/Publish: bind mount
        │  (privileged, bidir mounts)  │  (symlink fallback in dev mode)
        └───────────────┬──────────────┘
                        ▼
                     pods / PVCs
```

Metrics (`snapvol_op_duration_seconds`, `snapvol_op_errors_total`,
`snapvol_volumes`) and `/healthz` + `/readyz` are served on `:9808` (driver)
and `:8080` (control plane, API + metrics + probes).

## What works

- **CSI** (`csi.snapvol.dev`, spec via `container-storage-interface/spec`):
  `CreateVolume`/`DeleteVolume` (idempotent; clone from snapshot or volume),
  `ControllerPublish/Unpublish`, `NodeStage/Unstage`, `NodePublish/Unpublish`
  (bind mount of the backend path — an NFS export in prod, a local backing
  dir in dev mode), `CreateSnapshot`/`DeleteSnapshot`/`ListSnapshots`,
  `ControllerExpandVolume`, `GetCapacity`, `ValidateVolumeCapabilities`.
  Passes the `kubernetes-csi/csi-test` sanity suite in dev mode.
- **Control plane** (REST, [OpenAPI](api/openapi.yaml)): volumes, snapshots,
  snapshot schedules, jobs. Creates are idempotent by `request_id`
  (replayed responses carry `Idempotent-Replay: true`), byte/volume quota
  is enforced (`429` when exceeded), and schedules run through an async job
  queue with up to 3 attempts and backoff.
- **Backends**: `local` is fully implemented (state persisted as JSON next
  to the data, survives restarts). `fsx`, `azurefiles`, `gcpfilestore`
  satisfy the same `backend.Backend` interface and return
  `ErrNotImplemented` until their SDKs are wired in.

## Run locally (dev mode)

```sh
go run ./cmd/snapvol-csi -endpoint unix:///tmp/csi.sock -data-dir /tmp/snapvol &
go run ./cmd/controlplane -addr :8080 -data-dir /tmp/snapvol
curl -XPOST localhost:8080/v1/volumes -d '{"request_id":"r1","name":"data","size_bytes":1073741824}'
curl -XPOST localhost:8080/v1/snapshots -d '{"name":"s1","volume_id":"data"}'
curl localhost:9808/metrics | grep snapvol_
```

## Test

```sh
go vet ./... && go test -race ./...   # unit (table tests + fakes via temp dirs) + CSI sanity suite
make bench                            # latency benchmark, see below
./hack/e2e-kind.sh                    # kind: PVC write -> snapshot -> clone -> verify (needs kind/helm/kubectl)
```

CI (`.github/workflows/ci.yml`) runs vet, golangci-lint, `go test -race`, and
a Docker build on every push/PR; the kind e2e runs on pushes to `main`
(non-blocking — bind mounts in kind are flaky, see Limitations).

## Benchmark

`cmd/bench`, 1000 ops each against the local-fs backend (empty volumes;
snapshot source holds 4 KiB). Real output from the machine that produced
this README — yours will differ:

```
$ go run ./cmd/bench -ops 1000
snapvol-bench ops=1000 backend=local src_bytes=4096
CreateVolume  p50=84.5µs p99=182.333µs
CreateSnapshot p50=172.042µs p99=619.75µs
```

(Apple Silicon, darwin/arm64, Go 1.23.4, local SSD, 2026-11-18.)

## Deploy

```sh
docker build -t ghcr.io/ayushgawai/snapvol-csi:latest .   # distroless, static binaries
helm install snapvol deploy/helm -n kube-system           # controller Deployment + node DaemonSet,
                                                          # StorageClass, VolumeSnapshotClass, RBAC
```

An example (never applied by this repo) EKS cluster lives in
[`deploy/terraform/eks`](deploy/terraform/eks/main.tf):

```sh
cd deploy/terraform/eks && terraform init && terraform apply
```

## Limitations (honest list)

- **Local backend only.** Cloud backends are interface stubs; there is no
  real FSx/Azure/GCP provisioning, no credentials handling, no NFS server —
  in a real cluster the backing store must be shared (or node-local with
  topology constraints) for `ControllerPublish` paths to mean anything.
- **Snapshots are full directory copies** — O(volume size), no
  copy-on-write, no incremental/delta snapshots, no cross-region copies.
- **Single-node semantics.** Volume IDs are volume names; state is JSON
  files on local disk guarded by one mutex. No HA, no leader election, no
  distributed locking — two controller replicas would corrupt each other.
- **No real capacity accounting** on local dirs: sizes are bookkeeping for
  quota, not enforcement; `GetCapacity` reports a fixed 1 TiB.
- **Bind mounts need privilege** (`mount --bind`); unprivileged/dev mode
  silently falls back to symlinks, which do not behave identically (e.g.
  `NodeGetVolumeStats`, unmount semantics).
- **Control plane has no auth/TLS and in-memory idempotency/job state** —
  restarts lose schedules and replay history (volumes/snapshots persist).
- Expand is metadata-only locally; shrinking is rejected, cloud resize
  semantics (FSx etc.) are unimplemented.
- The kind e2e is documented and wired in CI as non-blocking; it has not
  been run against a managed cluster.

## License

MIT — see [LICENSE](LICENSE).
