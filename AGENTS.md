# AGENTS.md

Go CSI block driver (single binary, `cmd/main.go`) for Confidential Containers peer pods: creates cloud block volumes (AWS EBS / Azure Managed Disks / libvirt raw files) and writes `mountInfo.json` to the Kata shared dir so CAA can attach them to PodVMs. Architecture and volume lifecycle details: `docs/architecture.md`.

## Commands

- `make build` — CGO_ENABLED=0 binary into `bin/` (version from `git describe --tags --always --dirty`).
- `make fmt` / `make lint` — gofmt + `go vet`. That is the whole lint setup; no golangci config.
- `go test ./...` — unit tests (in `pkg/driver/`, `pkg/provider/azure/`).
- `make test` — **not** unit tests: builds the binary and runs csi-sanity conformance via `hack/run-csi-sanity.sh` against a temp libvirt provider (needs `csi-sanity`, auto-installed via `go install .../csi-test/v5/cmd/csi-sanity@latest`). Verbose: `make test-verbose`.
- Verification order: `make fmt && make lint && go test ./... && make test`.
- `make image` — builds with **podman** (not docker); Dockerfile `COPY`s `bin/caa-csi-block-driver`, so the binary must exist.

## CI reality

CI (`.github/workflows/`) only runs csi-sanity conformance and image build on PRs to `main` — **unit tests and go vet are not run in CI**. Run them locally before pushing.

## Provider pattern

Providers live in `pkg/provider/<name>/` and are selected by the StorageClass `cloudProvider` parameter via the factory registry (`pkg/provider/factory.go`). Adding one means: implement `BlockVolumeProvider` (`pkg/provider/interface.go`), call `provider.RegisterProvider(...)` in `init()`, and **blank-import the package in `cmd/main.go`** — without the import the registry lookup fails at runtime with "unsupported cloud provider".

## Gotchas

- Out-of-scope CSI RPCs (snapshot, expand, clone, list, publish/unpublish, stats fallback) are intentionally unsupported; keep the ginkgo skip list in `hack/run-csi-sanity.sh` in sync with `.github/workflows/csi-sanity.yaml` when touching RPC scope.
- Driver behavior is heavily env-driven: `KATA_DIRECT_VOLUME_ROOT_PATH`, `CSI_VOLUME_STORE_DIR`, `CSI_ALLOW_HOST_STATS_FALLBACK`, `CSI_TOPOLOGY_REGION/ZONE`, and the bootstrap-ConfigMap env vars in `pkg/driver/volumestore.go` (with `CSI_*` / legacy unprefixed fallbacks). Check `volumestore.go` and `nodeserver.go` before adding new config.
- `deploy/*.yaml` raw manifests and `charts/caa-csi-block-driver/` Helm templates duplicate the same resources — update both when changing deployment config (e.g., env vars, bootstrap ConfigMap).
