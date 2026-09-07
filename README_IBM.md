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
| `profile` / `ibmProfile` | VPC Storage Profile to use | `"general-purpose"`, `"custom"`, `"10iops-tier"` |
| `region` / `ibmRegion` | IBM Cloud region where volumes are provisioned | `"us-south"` |
| `zone` / `ibmZone` | IBM Cloud Availability Zone | `"us-south-1"` |
| `resourceGroup` / `ibmResourceGroup` | Resource Group ID for VPC volumes | `"your-resource-group-id"` |
| `iops` / `ibmIops` | Custom IOPS rate (only used with `"custom"` profile) | `"3000"` |
| `billingType` | Billing policy | `"hourly"` (default) or `"monthly"` |
| `encrypted` | Enable Key Protect / Hyper Protect encryption | `"true"` or `"false"` |
| `encryptionKey` | CRN of Key Protect key | `"crn:v1:bluemix:public:kms:..."` |
| `tags` | Comma-separated list of tags to append to the volume | `"confidential,caa-csi"` |
| `csi.storage.k8s.io/fstype` | Default filesystem used | `"ext4"` (default) or `"xfs"` |

---

## 4. Deploying to IBM IKS / ROKS

### Step 1: Clone and Set Up the Namespace
Ensure you have targeted the correct cluster context and apply the driver namespace and RBAC:

```bash
kubectl apply -f deploy/namespace.yaml
kubectl apply -f deploy/rbac.yaml
kubectl apply -f deploy/csi-driver.yaml
```

> **Note**: The driver dynamically reads its IAM API keys and configuration from the cluster's default `storage-secret-store` secret (mounted at `/etc/storage_ibmc/` via the `SECRET_CONFIG_PATH` environment variable). Ensure that this secret is present in the `caa-csi-block` namespace, or copy/replicate it over from your cluster's `kube-system` namespace if needed:
>
> ```bash
> # Copy storage-secret-store from kube-system to caa-csi-block namespace
> kubectl get secret storage-secret-store -n kube-system -o yaml | \
>   sed 's/namespace: kube-system/namespace: caa-csi-block/' | \
>   kubectl apply -f -
> ```

### Step 2: Deploy the DaemonSet
Update `deploy/daemonset-ibmcloud.yaml` to point to the built driver image that you pushed in Step 2, then deploy it:

```bash
kubectl apply -f deploy/daemonset-ibmcloud.yaml
```

### Step 3: Create the StorageClass
Apply the StorageClass to your cluster:

```bash
kubectl apply -f deploy/storageclass-ibmcloud.yaml
```

---

## 5. Verifying & Testing

Create a test PVC and map it to a sandboxed PeerPod to verify that volume creation, publishing, mounting, and deletion occur flawlessly.

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
      storage: 10Gi # Minimum supported VPC Block size is 10GiB
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

# Verify that the caa-csi-block-plugin controller logs volume creation
kubectl logs -n caa-csi-block -l app=caa-csi-block -c caa-csi-block-driver

# Verify the pod goes to 'Running' (attached directly to the PodVM)
kubectl get pod ibmcloud-test-pod
```
