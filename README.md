# CAA CSI Block Driver

A lightweight, pluggable CSI (Container Storage Interface) block driver for
[Confidential Containers](https://github.com/confidential-containers) Peer Pods.
It creates and manages block volumes across multiple cloud providers and integrates
with the [Cloud API Adaptor (CAA)](https://github.com/confidential-containers/cloud-api-adaptor)
for volume attachment to PodVMs.

## Supported Providers

| Provider | Backend | Authentication | Status |
|----------|---------|----------------|--------|
| **AWS** | EBS Volumes | IAM role or access key credentials | Supported |
| **Azure** | Managed Disks | DefaultAzureCredential (Workload Identity, Managed Identity, env vars) | Supported |
| **Libvirt** | Raw disk files | N/A (local) | Supported (dev/test) |

## Architecture

```
┌──────────────────────────────────────────────────────┐
│                  Kubernetes                          │
│  PVC ──► StorageClass ──► caa-csi-block-driver        │
│                              │                       │
│              ┌───────────────┼───────────────┐       │
│              │               │               │       │
│          ┌───▼───┐     ┌─────▼─────┐   ┌─────▼────┐ │
│          │  AWS  │     │  Libvirt  │   │  Azure   │ │
│          │(EBS)  │     │ (raw disk)│   │(Managed  │ │
│          │       │     │           │   │  Disks)  │ │
│          └───┬───┘     └─────┬─────┘   └────┬─────┘ │
│              │               │              │        │
│              └───────┬───────┴──────────────┘        │
│                      ▼                               │
│              mountInfo.json                          │
│                      │                               │
│                      ▼                               │
│           Cloud API Adaptor (CAA)                    │
│                      │                               │
│                      ▼                               │
│                   PodVM                              │
└──────────────────────────────────────────────────────┘
```

### How It Works

1. A PVC is created referencing a `StorageClass` backed by this driver
2. The **Controller Server** calls the appropriate cloud provider to create a block volume
3. The **Node Server** writes `mountInfo.json` to the Kata Containers shared directory
4. **CAA** reads `mountInfo.json` and attaches the volume to the PodVM
5. On PVC deletion, the controller deletes the cloud volume

## Project Structure

```
├── cmd/                    # Driver entrypoint
├── pkg/
│   ├── driver/             # CSI gRPC servers (controller, node, identity)
│   └── provider/
│       ├── interface.go    # BlockVolumeProvider interface
│       ├── factory.go      # Provider registry and factory
│       ├── aws/            # AWS EBS provider
│       ├── azure/          # Azure Managed Disks provider
│       └── libvirt/        # Libvirt raw disk provider
├── deploy/                 # Kubernetes manifests
├── hack/                   # Helper scripts
├── .github/workflows/      # CI pipelines
├── Dockerfile
├── Makefile
└── go.mod
```

## Container Image

Pre-built images are published to GHCR on every push to `main` and on version tags:

```bash
docker pull ghcr.io/confidential-devhub/caa-csi-block-driver:main
```

## Building

```bash
# Build the binary
make build

# Build the container image
make image

# Build for a specific platform
make build GOOS=linux GOARCH=amd64
```

## Deployment

### Prerequisites

- Kubernetes cluster with Kata Containers and CAA deployed
- `kata-remote` RuntimeClass configured

### AWS (EBS)

```bash
# Create the AWS credentials secret
kubectl create secret generic caa-csi-aws-creds \
  -n caa-csi-block \
  --from-literal=AWS_ACCESS_KEY_ID=<your-key> \
  --from-literal=AWS_SECRET_ACCESS_KEY=<your-secret>

kubectl apply -f deploy/namespace.yaml
kubectl apply -f deploy/rbac.yaml
kubectl apply -f deploy/csi-driver.yaml
kubectl apply -f deploy/daemonset-aws.yaml
kubectl apply -f deploy/storageclass-aws.yaml
```

### Azure (Managed Disks)

Authentication uses `DefaultAzureCredential`, which supports Workload Identity,
Managed Identity, and environment variables. No secrets in StorageClass parameters.

```bash
kubectl apply -f deploy/namespace.yaml
kubectl apply -f deploy/rbac.yaml
kubectl apply -f deploy/csi-driver.yaml
kubectl apply -f deploy/daemonset-azure.yaml

# Edit storageclass-azure.yaml with your subscription, resource group, and location
kubectl apply -f deploy/storageclass-azure.yaml
```

### Libvirt (local development)

```bash
kubectl apply -f deploy/namespace.yaml
kubectl apply -f deploy/rbac.yaml
kubectl apply -f deploy/csi-driver.yaml
kubectl apply -f deploy/daemonset.yaml
kubectl apply -f deploy/storageclass-libvirt.yaml
```

### Testing a Volume

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: test-pvc
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 1Gi
  storageClassName: caa-block-azure   # or caa-block-aws, caa-block-libvirt
---
apiVersion: v1
kind: Pod
metadata:
  name: test-pod
spec:
  runtimeClassName: kata-remote
  containers:
  - name: app
    image: busybox
    command: ["sh", "-c", "echo hello > /data/test.txt && sleep 3600"]
    volumeMounts:
    - name: vol
      mountPath: /data
  volumes:
  - name: vol
    persistentVolumeClaim:
      claimName: test-pvc
```

## Adding a New Provider

Implement the `BlockVolumeProvider` interface:

```go
type BlockVolumeProvider interface {
    CreateVolume(volumeID string, sizeBytes int64) (*VolumeInfo, error)
    DeleteVolume(volumeID string) error
    GetVolumeInfo(volumeID string) (*VolumeInfo, error)
    VolumeExists(volumeID string) (bool, error)
}
```

Then register it in an `init()` function:

```go
func init() {
    provider.RegisterProvider("myprovider", func(params map[string]string) (provider.BlockVolumeProvider, error) {
        return NewMyProvider(params)
    })
}
```

Import the package in `cmd/main.go`:

```go
_ "github.com/confidential-devhub/caa-csi-block-driver/pkg/provider/myprovider"
```

## Testing

```bash
# Run CSI conformance tests locally
make test

# Run tests with verbose output
make test-verbose
```

### Conformance Test Results

The driver passes all applicable [csi-sanity](https://github.com/kubernetes-csi/csi-test)
conformance tests:

- **33 Passed** — all tests for implemented CSI RPCs
- **58 Skipped** — optional features not in scope (snapshots, expansion, cloning, listing)

## License

Apache License 2.0 — see [LICENSE](LICENSE) for details.
