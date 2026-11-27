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
| **IBM Cloud VPC** | VPC Block Storage | API key in a Kubernetes Secret | Supported |

The following applies to every provider:

- The manifests and Helm chart deploy the `csi-provisioner` and
  `node-driver-registrar` sidecars. The IBM Cloud VPC install also deploys
  `csi-snapshotter`, because it is the only provider that implements
  snapshots. `csi-resizer` is not deployed for any provider, so volume
  expansion is not available.
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
│  │      AWS       │  │    Libvirt     │  │     Azure      │  │  IBM VPC Block ││
│  │     (EBS)      │  │   (raw disk)   │  │(Managed Disks) │  │                ││
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

The IBM Cloud provider authenticates with credentials you provide in a Secret
named `ibm-cloud-credentials` in the driver's namespace. Create the namespace
and the Secret before installing the driver.

```bash
kubectl apply -f deploy/namespace.yaml
```

Create a file `ibm-cloud-credentials.yaml` with an API key:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: ibm-cloud-credentials
  namespace: caa-csi-block
type: Opaque
stringData:
  ibm-credentials.env: |
    IBMCLOUD_AUTHTYPE=iam
    IBMCLOUD_APIKEY=<your-api-key>
```

Then apply it:

```bash
kubectl apply -f ibm-cloud-credentials.yaml
```

| Line | Value |
|------|-------|
| `IBMCLOUD_AUTHTYPE` | `iam` |
| `IBMCLOUD_APIKEY` | IBM Cloud API key |

The API key needs permission to manage VPC block storage
volumes and snapshots in the resource group you use.

Install the chart:

```bash
helm install caa-csi charts/caa-csi-block-driver \
  --namespace caa-csi-block \
  --set namespace.create=false \
  --set provider=ibmcloud \
  --set kubeletDir=/var/data/kubelet \
  --set image.repository=<your-registry>/caa-csi-block-driver \
  --set image.tag=<your-tag> \
  --set ibmcloud.resourceGroup=<resource-group-id> \
  --set ibmcloud.region=us-south \
  --set ibmcloud.zone=us-south-1 \
  --set storageClass.name=caa-csi-ibmcloud \
  --set storageClass.allowVolumeExpansion=false \
  --set serviceAccount.name=caa-csi-provisioner
```

The chart defaults to `provider: aws`, so `provider=ibmcloud` is required, as
are `ibmcloud.region`, `ibmcloud.zone` and `ibmcloud.resourceGroup`. Worker
nodes keep kubelet data under `/var/data/kubelet`, which is why `kubeletDir` is
set.

`ibmcloud.zone` must be the zone CAA creates PodVMs in (`IBMCLOUD_ZONE` in the CAA
configuration). CAA refuses to attach a volume from any other zone, and the pod
stays in `ContainerCreating`.

The driver calls the public VPC endpoint for the region. To use the private
endpoint, add `--set ibmcloud.vpcEndpointURL=https://<region>.private.iaas.cloud.ibm.com`.

On ROKS, grant the privileged SCC to the service account:

```bash
oc adm policy add-scc-to-user privileged -z caa-csi-provisioner -n caa-csi-block
```

#### Install with manifests

As an alternative to the chart, after creating the Secret above:

```bash
kubectl apply -f deploy/rbac.yaml
kubectl apply -f deploy/csi-driver.yaml
# Edit bootstrap-params-ibmcloud.yaml with your region, zone and resource group first
kubectl apply -f deploy/bootstrap-params-ibmcloud.yaml
# Edit daemonset-ibmcloud.yaml to point at your driver image first
kubectl apply -f deploy/daemonset-ibmcloud.yaml
# Edit storageclass-ibmcloud.yaml with your region, zone and resource group first
kubectl apply -f deploy/storageclass-ibmcloud.yaml
```

`deploy/rbac.yaml` already grants the privileged SCC on OpenShift, so the `oc adm policy` step is not needed here.

#### StorageClass parameters

| Key | Description | Example |
|-----|-------------|---------|
| `profile` | VPC storage profile: `general-purpose`, `5iops-tier`, `10iops-tier`, `custom`, `sdp` | `general-purpose` |
| `region` | Region for volumes | `us-south` |
| `zone` | Availability zone; must belong to `region` and be the zone CAA creates PodVMs in | `us-south-1` |
| `resourceGroup` | Resource group ID (required) | |
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

#### Snapshots

The IBM Cloud VPC install runs the `csi-snapshotter` sidecar, so you can take a
snapshot of a volume and create a new volume from it. The cluster must already
have the snapshot CRDs and the snapshot controller (OpenShift ships both).

```yaml
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshotClass
metadata:
  name: caa-csi-ibmcloud-snapshot
driver: caa-csi-block.csi.confidentialcontainers.io
deletionPolicy: Delete
---
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: my-snapshot
spec:
  volumeSnapshotClassName: caa-csi-ibmcloud-snapshot
  source:
    persistentVolumeClaimName: <pvc-name>
```

To restore, create a PVC with the snapshot as its `dataSource`:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: my-restore
spec:
  accessModes:
    - ReadWriteOnce
  storageClassName: caa-csi-ibmcloud
  resources:
    requests:
      storage: 20Gi
  dataSource:
    name: my-snapshot
    kind: VolumeSnapshot
    apiGroup: snapshot.storage.k8s.io
```

#### Verify

```bash
kubectl get storageclass caa-csi-ibmcloud
kubectl get pvc <pvc-name>     # expect Bound
kubectl get pod <pod-name>     # expect Running

# Helm install
kubectl get pods -n caa-csi-block -l app.kubernetes.io/instance=caa-csi
kubectl logs -n caa-csi-block -l app.kubernetes.io/name=caa-csi-block-driver -c caa-csi-block-driver

# Manifest install
kubectl get pods -n caa-csi-block -l app=caa-csi-block
kubectl logs -n caa-csi-block -l app=caa-csi-block -c caa-csi-block-driver
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