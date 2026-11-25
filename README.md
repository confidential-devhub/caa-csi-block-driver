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
| **IBM Cloud VPC** | VPC Block Storage | Cluster credential store (`storage-secret-store`) | Supported (tested on ROKS) |

### Features

| Feature | AWS | Azure | IBM Cloud VPC | Libvirt |
|---------|-----|-------|---------------|---------|
| Create / delete volume | Yes | Yes | Yes | Yes |
| Volume expansion | Yes | Yes | Yes | Yes |
| Snapshots (create, delete, list) | Yes | Yes | Yes | No |
| Volume from snapshot | Yes | Yes | Yes | No |
| Volume clone | Yes | Yes | Yes | Yes |
| Volume recovery after store loss | Yes | Yes | Yes | Yes |

The following applies to every provider:

- The manifests and Helm chart deploy the `csi-provisioner` and
  `node-driver-registrar` sidecars only. They do not deploy `csi-snapshotter`
  or `csi-resizer`.
- `ControllerPublishVolume` / `ControllerUnpublishVolume` are not implemented by
  design: CAA attaches disks through the cloud API.
- `ListVolumes`, `GetCapacity`, `ModifyVolume` and the group controller service
  are not implemented.

  
## Architecture

```
┌────────────────────────────────────────────────────────────────────────────────┐
│                                    Kubernetes                                  │
│  PVC ──► StorageClass ──► caa-csi-block-driver                                 │
│                                         │                                      │
│           ┌───────────────────┬─────────┴─────────┬───────────────────┐        │
│  ┌────────────────┐  ┌────────────────┐  ┌────────────────┐  ┌────────────────┐│
│  │      AWS       │  │    Libvirt     │  │     Azure      │  │    IBM VPC     ││
│  │     (EBS)      │  │   (raw disk)   │  │(Managed Disks) │  │  (VPC Block)   ││
│  └────────────────┘  └────────────────┘  └────────────────┘  └────────────────┘│
│           │                   │                   │                   │        │
│           └───────────────────┴─────────┬─────────┴───────────────────┘        │
│                                         ▼                                      │
│                                  mountInfo.json                                │
│                                         ▼                                      │
│                              Cloud API Adaptor (CAA)                           │
│                                         ▼                                      │
│                                       PodVM                                    │
└────────────────────────────────────────────────────────────────────────────────┘
```

### How It Works

1. A PVC is created referencing a `StorageClass` backed by this driver
2. The **Controller Server** calls the appropriate cloud provider to create a block volume
3. The **Node Server** writes `mountInfo.json` to the Kata Containers shared directory
4. **CAA** reads `mountInfo.json` and attaches the volume to the PodVM
5. On PVC deletion, the controller deletes the cloud volume

See [docs/architecture.md](docs/architecture.md) for details on expansion,
snapshots and recovery.

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
│       ├── ibmcloud/       # IBM Cloud VPC Block provider
│       └── libvirt/        # Libvirt raw disk provider
├── charts/                 # Helm chart
├── deploy/                 # Kubernetes manifests
├── docs/                   # Architecture and platform guides
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
kubectl apply -f deploy/namespace.yaml

# Create the AWS credentials secret
kubectl create secret generic caa-csi-aws-creds \
  -n caa-csi-block \
  --from-literal=AWS_ACCESS_KEY_ID=<your-key> \
  --from-literal=AWS_SECRET_ACCESS_KEY=<your-secret>

# Optional: deploy bootstrap ConfigMap for empty-store recovery
# Edit bootstrap-params-aws.yaml with your region and availability zone first
kubectl apply -f deploy/bootstrap-params-aws.yaml

kubectl apply -f deploy/rbac.yaml
kubectl apply -f deploy/csi-driver.yaml
kubectl apply -f deploy/daemonset-aws.yaml
kubectl apply -f deploy/storageclass-aws.yaml
```

The default AWS StorageClass sets `awsAvailabilityZone` to match a typical
CAA `AWS_SUBNET_ID` AZ (edit it). To let the driver take the zone from the
scheduled node instead, use `deploy/storageclass-aws-topology.yaml` — only
when the PodVM AZ is the same as the worker AZ. See
[docs/architecture.md](docs/architecture.md#topology-aware-provisioning).

### Azure (Managed Disks)

Authentication uses `DefaultAzureCredential`, which supports Workload Identity,
Managed Identity, and environment variables. No secrets in StorageClass parameters.

```bash
kubectl apply -f deploy/namespace.yaml
kubectl apply -f deploy/rbac.yaml
kubectl apply -f deploy/csi-driver.yaml

# Optional: deploy bootstrap ConfigMap for empty-store recovery
# Edit bootstrap-params-azure.yaml with your subscription, resource group, and location first
kubectl apply -f deploy/bootstrap-params-azure.yaml

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

### IBM Cloud VPC

The IBM Cloud provider reads the `storage-secret-store` Secret and the
`cluster-info` ConfigMap from the namespace the driver runs in. The cluster
creates both in `kube-system` only, so copy them into `caa-csi-block` first.
Re-copy them if the originals change:

```bash
kubectl apply -f deploy/namespace.yaml

for obj in secret/storage-secret-store configmap/cluster-info; do
  kubectl -n kube-system get "$obj" -o json |
  jq 'del(.metadata.uid, .metadata.resourceVersion, .metadata.creationTimestamp, .metadata.managedFields) |
      .metadata.namespace = "caa-csi-block"' |
  kubectl apply -f -
done
```

If `storage-secret-store` uses an IAM trusted profile, authorize the driver's
service account (`system:serviceaccount:caa-csi-block:caa-csi-provisioner`) in
IBM Cloud IAM.

Install the chart:

```bash
helm install caa-csi charts/caa-csi-block-driver \
  --namespace caa-csi-block \
  --set namespace.create=false \
  --set provider=ibmcloud \
  --set kubeletDir=/var/data/kubelet \
  --set image.repository=<your-registry>/caa-csi-block-driver \
  --set image.tag=<your-tag> \
  --set ibmcloud.region=us-south \
  --set ibmcloud.zone=us-south-1 \
  --set storageClass.name=caa-csi-ibmcloud \
  --set storageClass.allowVolumeExpansion=false \
  --set serviceAccount.name=caa-csi-provisioner
```

The chart defaults to `provider: aws`, so `provider=ibmcloud` is required, as
are `ibmcloud.region` and `ibmcloud.zone`. Worker nodes keep kubelet data under
`/var/data/kubelet`, which is why `kubeletDir` is set.

On ROKS, grant the privileged SCC to the service account:

```bash
oc adm policy add-scc-to-user privileged -z caa-csi-provisioner -n caa-csi-block
```

The raw manifests `deploy/daemonset-ibmcloud.yaml` and `deploy/rbac.yaml` target
`kube-system` instead, where the credential objects already exist. The Helm
install above is the tested path.

#### StorageClass parameters

| Key | Description | Example |
|-----|-------------|---------|
| `profile` | VPC storage profile: `general-purpose`, `5iops-tier`, `10iops-tier`, `custom`, `sdp` | `general-purpose` |
| `region` | Region for volumes | `us-south` |
| `zone` | Availability zone; must belong to `region` | `us-south-1` |
| `resourceGroup` | Resource group ID; omit to use the one in `storage-secret-store` | |
| `iops` | IOPS, for `custom` and `sdp` only | `3000` |
| `throughput` | Bandwidth in Mbps, for `sdp` only | `2000` |
| `billingType` | `hourly` or `monthly` | `hourly` |
| `encrypted` | `"true"` or `"false"` | `"false"` |
| `encryptionKey` | Key Protect key CRN; required when `encrypted` is `"true"` | `crn:v1:bluemix:public:kms:...` |
| `tags` | Comma-separated tags added to each volume | `confidential,caa-csi` |
| `csi.storage.k8s.io/fstype` | Filesystem | `ext4` or `xfs` |

`ibmProfile`, `ibmRegion`, `ibmZone`, `ibmResourceGroup` and `ibmIops` are
accepted as fallbacks for the matching keys.

Requested sizes are rounded up to a whole GiB. The minimum is 10 GiB, or 1 GiB
for the `sdp` profile.

#### Verify

```bash
kubectl get pods -n caa-csi-block -l app.kubernetes.io/instance=caa-csi
kubectl get storageclass caa-csi-ibmcloud
kubectl get pvc <pvc-name>     # expect Bound
kubectl get pod <pod-name>     # expect Running
kubectl logs -n caa-csi-block -l app.kubernetes.io/name=caa-csi-block-driver -c caa-csi-block-driver
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
  storageClassName: caa-block-azure   # or caa-block-aws, caa-block-libvirt, caa-csi-ibmcloud
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

For IBM Cloud VPC, request at least `10Gi` unless the StorageClass uses the `sdp` profile.

## Adding a New Provider

Implement the `BlockVolumeProvider` interface:

```go
type BlockVolumeProvider interface {
    CreateVolume(ctx context.Context, volumeID string, sizeBytes int64) (*VolumeInfo, error)
    DeleteVolume(ctx context.Context, volumeID string) error
    GetVolumeInfo(ctx context.Context, volumeID string) (*VolumeInfo, error)
    VolumeExists(ctx context.Context, volumeID string) (bool, error)
}
```

A provider can also implement these optional interfaces from
`pkg/provider/interface.go`:

| Interface | Adds |
|-----------|------|
| `VolumeExpander` | Volume expansion |
| `VolumeSnapshotter` | Snapshot create, delete, list and find |
| `VolumeCloner` | Volume from a snapshot or from another volume |
| `VolumeRecoverer` | Listing managed volumes for store recovery |

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