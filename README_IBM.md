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

The IBM libraries read the `storage-secret-store` Secret and `cluster-info`
ConfigMap from the namespace the driver pod runs in. IKS and ROKS create both
in `kube-system` only, so deploy the driver there. If the cluster's
`storage-secret-store` uses an IAM trusted profile, that profile must also
authorize the driver's service account.

IKS and ROKS on VPC use `/var/data/kubelet` as the kubelet root directory.

### Installing with Helm

```bash
helm install caa-csi charts/caa-csi-block-driver \
  --namespace kube-system \
  --set namespace.name=kube-system \
  --set namespace.create=false \
  --set provider=ibmcloud \
  --set kubeletDir=/var/data/kubelet \
  --set image.repository=<your-registry>/caa-csi-block-driver \
  --set image.tag=v0.2.0 \
  --set ibmcloud.region=us-south \
  --set ibmcloud.zone=us-south-1
```

On ROKS, also grant the privileged SCC to the chart's service account:

```bash
oc adm policy add-scc-to-user privileged -z caa-csi-caa-csi-block-driver -n kube-system
```

See the `ibmcloud` section of `charts/caa-csi-block-driver/values.yaml` for
the remaining StorageClass settings.

### Installing with Manifests

### Step 1: Clone and Set Up the Namespace
Ensure you have targeted the correct cluster context and apply the driver RBAC:

```bash
kubectl apply -f deploy/rbac.yaml
kubectl apply -f deploy/csi-driver.yaml
```

### Step 2: Deploy the DaemonSet
Update `deploy/daemonset-ibmcloud.yaml` to point to the built driver image, then deploy it:

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

# Verify the driver logs volume creation (Helm installs use -l app.kubernetes.io/name=caa-csi-block-driver)
kubectl logs -n kube-system -l app=caa-csi-block -c caa-csi-block-driver

# Verify the pod goes to 'Running' with the PeerPod runtime
kubectl get pod ibmcloud-test-pod
```
