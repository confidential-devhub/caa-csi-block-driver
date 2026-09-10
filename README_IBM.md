# CAA CSI Block Driver — IBM Cloud VPC Block Storage Provider

This guide describes how to build, deploy, and verify the `caa-csi-block-driver` with the **IBM Cloud VPC Block Storage provider** on an IBM Cloud IKS (IBM Cloud Kubernetes Service) or ROKS (Red Hat OpenShift on IBM Cloud) cluster.

The IBM Cloud provider integrates with the same credential infrastructure used by the community `ibm-vpc-block-csi-driver` in IKS/ROKS clusters. It dynamically accesses credentials from the cluster's secret store (`storage-secret-store`), removing the need for raw api keys inside `StorageClass` parameters.

---

## 1. Project Structure

The IBM Cloud provider resides in the following locations within this repository:

- **Provider Logic**: `pkg/provider/ibmcloud/provider.go`
- **Driver Main registration**: `cmd/main.go`
- **Deployment Manifests**:
  - `deploy/daemonset-ibmcloud.yaml`
  - `deploy/storageclass-ibmcloud.yaml`

---

## 2. Building the Driver

To compile the driver binary and containerize it, follow these steps:

### Build the Binary
Compile the binary for Linux AMD64 (the standard deployment architecture for Kubernetes worker nodes and PeerPod VMs):

```bash
# Build the binary
make build GOOS=linux GOARCH=amd64
```

This compiles the static binary and places it under `bin/caa-csi-block-driver`.

### Build and Push the Container Image
The `Dockerfile` in the root of the repository copies this binary into a lightweight Alpine image.

```bash
# Build the container image using Podman or Docker
podman build -t <your-registry>/caa-csi-block-driver:v0.2.0 .

# Log in and push the image to your container registry (e.g., IBM Cloud Container Registry or Quay)
podman push <your-registry>/caa-csi-block-driver:v0.2.0
```

---

## 3. Configuration & StorageClass Parameters

The provider supports both the standard Kubernetes-SIG community keys and an alternative `ibm`-prefixed fallback, making it fully compatible with standard Helm charts and cluster settings.

### StorageClass Parameters Supported

| Key | Description | Example / Default |
| --- | --- | --- |
| `profile` / `ibmProfile` | VPC Storage Profile to use | `"general-purpose"`, `"5iops-tier"`, `"10iops-tier"`, `"custom"`, `"sdp"` |
| `region` / `ibmRegion` | IBM Cloud region where volumes are provisioned | `"us-south"` |
| `zone` / `ibmZone` | IBM Cloud Availability Zone | `"us-south-1"` |
| `resourceGroup` / `ibmResourceGroup` | Optional Resource Group ID override | Provider-configured group when omitted |
| `iops` / `ibmIops` | Optional IOPS for `custom` or `sdp`; ignored for tiered profiles as upstream does | `"3000"`; omitted values use SDK/API behavior |
| `throughput` | Requested bandwidth in Mbps (for `sdp`), passed as SDK `Bandwidth` | `"2000"`; omitted uses cloud defaults |
| `billingType` | Billing policy | `"hourly"` (default) or `"monthly"` |
| `encrypted` | Enable Key Protect / Hyper Protect encryption | `"true"` or `"false"` |
| `encryptionKey` | CRN of Key Protect key | `"crn:v1:bluemix:public:kms:..."` |
| `tags` | Comma-separated list of tags to append to the volume | `"confidential,caa-csi"` |
| `csi.storage.k8s.io/fstype` | Default filesystem used | `"ext4"` (default) or `"xfs"` |

SDP volumes can start at 1 GiB. Other profiles require at least 10 GiB. The IBM
VPC provider validates supported profile, capacity, IOPS, and throughput combinations.

---

## 4. Deploying to IBM IKS / ROKS

### Step 1: Clone and Set Up the Namespace
Ensure you have targeted the correct cluster context and apply the driver namespace and RBAC:

```bash
kubectl apply -f deploy/namespace.yaml
kubectl apply -f deploy/rbac.yaml
kubectl apply -f deploy/csi-driver.yaml
```

> The DaemonSet expects IBM authentication configuration in the
> `caa-csi-block` namespace. Kubernetes service accounts and secrets are
> namespace-scoped, so resources from `kube-system` are not reused automatically.
> Ensure the IBM IAM trusted profile authorizes
> `system:serviceaccount:caa-csi-block:caa-csi-provisioner`, and that both
> `storage-secret-store` and `cluster-info` exist in `caa-csi-block` before deploying.

### Step 2: Deploy the DaemonSet
Update `deploy/daemonset-ibmcloud.yaml` to point to the built driver image, then deploy it:

```bash
kubectl apply -f deploy/daemonset-ibmcloud.yaml
```

> **Note on Volume Stats**: By default, the Node Server attempts to gather filesystem metrics from inside the sandboxed PeerPod guest VM via the `kata-runtime` CLI. Since `kata-runtime` is not pre-packaged inside alpine-based CSI containers, the DaemonSet includes the environment variable `CSI_ALLOW_HOST_STATS_FALLBACK: "true"`. This instructs the Node Server to fallback gracefully to host-side `statfs` measurements of the mount point and prevents `"exec: \"kata-runtime\": executable file not found in $PATH"` errors from flooding the driver logs.

### Step 3: Create the StorageClass
Apply the StorageClass to your cluster:

```bash
kubectl apply -f deploy/storageclass-ibmcloud.yaml
```

---

## 5. Verifying & Testing

Create a test PVC and map it to a sandboxed PeerPod to verify volume creation,
publishing, mounting, and deletion.

Save the following as `test-pvc-pod.yaml`:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ibmcloud-test-pvc
  namespace: default
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 10Gi # Minimum supported VPC Block size for non-SDP profiles
  storageClassName: caa-csi-ibmcloud
---
apiVersion: v1
kind: Pod
metadata:
  name: ibmcloud-test-pod
  namespace: default
spec:
  runtimeClassName: kata-remote # Targets the PodVM / PeerPod runtime
  containers:
  - name: app
    image: busybox
    command: ["sh", "-c", "echo 'Hello from IBM Cloud' > /data/test.txt && sleep 3600"]
    volumeMounts:
    - name: data-vol
      mountPath: /data
  volumes:
  - name: data-vol
    persistentVolumeClaim:
      claimName: ibmcloud-test-pvc
```

Apply the configuration:

```bash
kubectl apply -f test-pvc-pod.yaml
```

Check the status of the volume and pod:

```bash
# Verify the PVC status goes to 'Bound'
kubectl get pvc ibmcloud-test-pvc

# Verify the driver logs volume creation
kubectl logs -n caa-csi-block -l app=caa-csi-block -c caa-csi-block-driver

# Verify the pod goes to 'Running' with the PeerPod runtime
kubectl get pod ibmcloud-test-pod
```
