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

SDP uses a 1 GiB minimum; tiered/custom profiles retain the 10 GiB minimum.
The driver rounds requests up to whole GiB. The VPC API validates available
profiles and capacity/performance combinations; no maximum-range table is
duplicated here. An SDP StorageClass can use `profile: "sdp"`, `iops: "3000"`
and `throughput: "2000"` for a 30 GiB test PVC (plus the normal placement fields).

References: [IBM StorageClass documentation](https://cloud.ibm.com/docs/openshift?topic=openshift-vpc-block#custom-sc),
[upstream parameter/default handling](https://github.com/kubernetes-sigs/ibm-vpc-block-csi-driver/blob/master/pkg/ibmcsidriver/controller_helper.go).

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
> Ensure `caa-csi-provisioner` is authorized for the cluster's IBM IAM setup and
> that `storage-secret-store` exists in `caa-csi-block` before deploying.

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

Save the following as `test-pvc.yaml`:

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
      storage: 10Gi # Minimum for non-SDP profiles
  storageClassName: caa-csi-ibmcloud
```

Create the volume and verify that the claim becomes `Bound`:

```bash
kubectl apply -f test-pvc.yaml
kubectl get pvc ibmcloud-test-pvc -w
```

Check the driver logs if provisioning fails:

```bash
kubectl logs -n caa-csi-block -l app=caa-csi-block -c caa-csi-block-driver
```

Delete the test claim. Because the supplied StorageClass uses
`reclaimPolicy: Delete`, the corresponding IBM VPC volume is also deleted:

```bash
kubectl delete -f test-pvc.yaml
```
