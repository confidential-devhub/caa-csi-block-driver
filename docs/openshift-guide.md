# Using Cloud CSI Adaptor with OpenShift Sandboxed Containers on ARO

This guide walks through setting up the Cloud CSI Adaptor with OpenShift Sandboxed Containers (OSC) on an Azure Red Hat OpenShift (ARO) cluster to provision Azure Managed Disks for Peer Pod workloads.

OSC is Red Hat's productized distribution of Kata Containers for OpenShift. It provides peer pod support through an Operator that manages the Cloud API Adaptor (CAA), kata runtime installation, PodVM image creation, and lifecycle management.

## Prerequisites

- Azure CLI (`az`) installed and logged in
- `oc` CLI installed ([download](https://mirror.openshift.com/pub/openshift-v4/clients/ocp/stable/))
- `helm` installed
- An Azure subscription with permissions to create ARO clusters and Managed Disks
- A Red Hat pull secret ([download](https://console.redhat.com/openshift/install/pull-secret))
- An Azure Service Principal (ARO does not support OIDC/workload identity)

## 1. Set Up Environment Variables

```bash
export RESOURCE_GROUP="caa-csi-aro"
export CLUSTER_NAME="caa-csi-aro-cluster"
export LOCATION="eastus"
export SUBSCRIPTION_ID=$(az account show --query id -o tsv)
export VNET_NAME="caa-csi-aro-vnet"
export PULL_SECRET_PATH="$HOME/pull-secret"
```

## 2. Create an ARO Cluster

### Create the resource group and virtual network

ARO requires a VNet with separate subnets for master and worker nodes:

```bash
az group create --name $RESOURCE_GROUP --location $LOCATION

az network vnet create \
  --resource-group $RESOURCE_GROUP \
  --name $VNET_NAME \
  --address-prefixes 10.0.0.0/22 10.0.4.0/24

az network vnet subnet create \
  --resource-group $RESOURCE_GROUP \
  --vnet-name $VNET_NAME \
  --name master-subnet \
  --address-prefixes 10.0.0.0/23

az network vnet subnet create \
  --resource-group $RESOURCE_GROUP \
  --vnet-name $VNET_NAME \
  --name worker-subnet \
  --address-prefixes 10.0.2.0/23
```

### Create the ARO cluster

Pass `--client-id` and `--client-secret` using a pre-existing Service Principal:

```bash
az aro create \
  --resource-group $RESOURCE_GROUP \
  --name $CLUSTER_NAME \
  --vnet $VNET_NAME \
  --master-subnet master-subnet \
  --worker-subnet worker-subnet \
  --client-id $SP_APP_ID \
  --client-secret $SP_PASSWORD \
  --pull-secret @$PULL_SECRET_PATH \
  --worker-count 3 \
  --worker-vm-size Standard_D4s_v3
```

> **Note:** ARO requires a minimum of 3 worker nodes. Cluster creation takes approximately 30-40 minutes.

### Connect to the cluster

```bash
API_URL=$(az aro show --name $CLUSTER_NAME --resource-group $RESOURCE_GROUP \
  --query "apiserverProfile.url" -o tsv)
KUBEADMIN_PASS=$(az aro list-credentials --name $CLUSTER_NAME \
  --resource-group $RESOURCE_GROUP --query "kubeadminPassword" -o tsv)

oc login $API_URL --username kubeadmin --password $KUBEADMIN_PASS \
  --insecure-skip-tls-verify
```

Verify the cluster is running:

```bash
oc get nodes
```

## 3. Create the Peer Pod Subnet

Create a dedicated subnet for peer pod VMs with a NAT gateway for internet access:

```bash
az network public-ip create \
  --resource-group $RESOURCE_GROUP \
  --name peerpod-ip \
  --location $LOCATION

az network nat gateway create \
  --resource-group $RESOURCE_GROUP \
  --location $LOCATION \
  --public-ip-addresses peerpod-ip \
  --name peerpod-nat

az network vnet subnet create \
  --resource-group $RESOURCE_GROUP \
  --vnet-name $VNET_NAME \
  --name peerpod-subnet \
  --address-prefixes 10.0.4.0/24 \
  --nat-gateway peerpod-nat

export AZURE_SUBNET_ID=$(az network vnet subnet show \
  --resource-group $RESOURCE_GROUP \
  --vnet-name $VNET_NAME \
  --name peerpod-subnet \
  --query id -o tsv)
```

## 4. Get the ARO Infrastructure Details

Retrieve the infrastructure resource group name and NSG:

```bash
export INFRA_RG=$(oc get infrastructure/cluster \
  -o jsonpath='{.status.platformStatus.azure.resourceGroupName}')

export AZURE_NSG_ID=$(az network nsg list \
  --resource-group $INFRA_RG \
  --query "[0].id" -o tsv)
```

## 5. Install OpenShift Sandboxed Containers Operator

### Create the Operator namespace

```bash
oc apply -f - <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: openshift-sandboxed-containers-operator
EOF
```

### Create the OperatorGroup and Subscription

```bash
oc apply -f - <<EOF
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: sandboxed-containers-operator-group
  namespace: openshift-sandboxed-containers-operator
spec:
  targetNamespaces:
  - openshift-sandboxed-containers-operator
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: sandboxed-containers-operator
  namespace: openshift-sandboxed-containers-operator
spec:
  channel: stable
  installPlanApproval: Automatic
  name: sandboxed-containers-operator
  source: redhat-operators
  sourceNamespace: openshift-marketplace
EOF
```

Wait for the Operator to install:

```bash
oc wait --for=jsonpath='{.status.phase}'=Succeeded csv \
  -n openshift-sandboxed-containers-operator \
  -l operators.coreos.com/sandboxed-containers-operator.openshift-sandboxed-containers-operator \
  --timeout=300s
```

## 6. Configure Peer Pods

### Create the credentials Secret

```bash
oc create secret generic peer-pods-secret \
  -n openshift-sandboxed-containers-operator \
  --from-literal=AZURE_CLIENT_ID=$SP_APP_ID \
  --from-literal=AZURE_CLIENT_SECRET=$SP_PASSWORD \
  --from-literal=AZURE_TENANT_ID=$SP_TENANT_ID \
  --from-literal=AZURE_SUBSCRIPTION_ID=$SUBSCRIPTION_ID
```

### Create the peer-pods ConfigMap

```bash
oc apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: peer-pods-cm
  namespace: openshift-sandboxed-containers-operator
data:
  CLOUD_PROVIDER: "azure"
  VXLAN_PORT: "9000"
  PROXY_TIMEOUT: "5m"
  AZURE_INSTANCE_SIZE: "Standard_D2as_v5"
  AZURE_INSTANCE_SIZES: "Standard_D2as_v5,Standard_D4as_v5"
  AZURE_SUBNET_ID: "${AZURE_SUBNET_ID}"
  AZURE_NSG_ID: "${AZURE_NSG_ID}"
  AZURE_REGION: "${LOCATION}"
  AZURE_RESOURCE_GROUP: "${RESOURCE_GROUP}"
  DISABLECVM: "true"
EOF
```

> **Note:** Set `DISABLECVM: "false"` and use a CVM-capable instance size (e.g., `Standard_DC2as_v5`) for AMD SEV-SNP confidential VMs.

## 7. Create the KataConfig

The KataConfig CR triggers the Operator to install the kata runtime on worker nodes and create the PodVM image. This process reboots each worker node and can take 30-60 minutes:

```bash
oc apply -f - <<EOF
apiVersion: kataconfiguration.openshift.io/v1
kind: KataConfig
metadata:
  name: example-kataconfig
spec:
  enablePeerPods: true
  logLevel: info
EOF
```

Monitor the installation progress:

```bash
watch oc describe kataconfig example-kataconfig
```

The installation is complete when:
- All nodes show as `Installed` in the status
- `kata-remote` appears in the `Runtime Classes` list
- The `osc-caa-ds` DaemonSet pods are all `Running`

```bash
oc get runtimeclass kata-remote
oc get pods -n openshift-sandboxed-containers-operator -l name=osc-caa-ds
```

> **Note:** The Operator automatically creates a PodVM image and uploads it to an Azure Compute Gallery in your resource group. If the image creation job fails (e.g., due to networking issues with private endpoints), check the job pod logs: `oc logs -n openshift-sandboxed-containers-operator -l job-name=osc-podvm-image-creation`

## 8. Deploy the CSI Driver

### Install via Helm

```bash
helm install cloud-csi-adaptor ./charts/cloud-csi-adaptor \
  --set provider=azure \
  --set azure.subscriptionId=$SUBSCRIPTION_ID \
  --set azure.resourceGroup=$RESOURCE_GROUP \
  --set azure.location=$LOCATION
```

> Replace `./charts/cloud-csi-adaptor` with the path to your local clone of the [cloud-csi-adaptor](https://github.com/confidential-devhub/cloud-csi-adaptor) repository.

### Grant privileged SCC

```bash
oc adm policy add-scc-to-user privileged \
  -z caa-csi-provisioner -n caa-csi-block
```

### Set Azure credentials

Since ARO does not support OIDC/workload identity, inject Service Principal credentials via environment variables:

```bash
oc create secret generic csi-azure-creds \
  -n caa-csi-block \
  --from-literal=AZURE_TENANT_ID=$SP_TENANT_ID \
  --from-literal=AZURE_CLIENT_ID=$SP_APP_ID \
  --from-literal=AZURE_CLIENT_SECRET=$SP_PASSWORD

oc set env ds/cloud-csi-adaptor -n caa-csi-block \
  --from=secret/csi-azure-creds -c cloud-csi-adaptor
```

Verify the CSI driver pods are running:

```bash
oc get pods -n caa-csi-block
```

All pods should show `3/3 Running`.

## 9. Test with a Peer Pod Workload

### Create a PVC

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: test-csi-pvc
spec:
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 1Gi
  storageClassName: caa-block-azure
```

```bash
oc apply -f test-pvc.yaml
```

### Create a test pod with kata-remote runtime

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: test-csi-pod
spec:
  runtimeClassName: kata-remote
  containers:
    - name: app
      image: busybox
      command: ["sh", "-c", "echo 'Hello from Peer Pod CSI on ARO!' > /data/test.txt && cat /data/test.txt && sleep 3600"]
      volumeMounts:
        - name: csi-vol
          mountPath: /data
  volumes:
    - name: csi-vol
      persistentVolumeClaim:
        claimName: test-csi-pvc
```

```bash
oc apply -f test-pod.yaml
```

### Verify

Wait for the pod to be running (PodVM creation takes 1-2 minutes):

```bash
oc wait --for=condition=Ready pod/test-csi-pod --timeout=300s
```

Check the logs:

```bash
oc logs test-csi-pod
# Expected output: Hello from Peer Pod CSI on ARO!
```

Verify the PodVM and Azure Managed Disk were created:

```bash
az vm list --resource-group $RESOURCE_GROUP --output table
az disk list --resource-group $RESOURCE_GROUP --output table
```

## 10. Cleanup

```bash
oc delete pod test-csi-pod
oc delete pvc test-csi-pvc
helm uninstall cloud-csi-adaptor
oc delete kataconfig example-kataconfig
az aro delete --name $CLUSTER_NAME --resource-group $RESOURCE_GROUP --yes
az group delete --name $RESOURCE_GROUP --yes --no-wait
```

## Troubleshooting

### CSI driver pods stuck in ImagePullBackOff

If the container image is hosted on a private registry, create a pull secret and patch the DaemonSet:

```bash
oc create secret docker-registry registry-secret \
  --docker-server=<registry> \
  --docker-username=<user> \
  --docker-password=<token> \
  -n caa-csi-block

oc patch ds cloud-csi-adaptor -n caa-csi-block --type=json \
  -p='[{"op":"add","path":"/spec/template/spec/imagePullSecrets","value":[{"name":"registry-secret"}]}]'
```

### DaemonSet pods not starting (SCC errors)

If events show `unable to validate against any security context constraint`, grant the privileged SCC to the service account:

```bash
oc adm policy add-scc-to-user privileged -z <service-account-name> -n <namespace>
```

### osc-caa-ds pods in CrashLoopBackOff

If the OSC CAA DaemonSet pods crash with `$AZURE_IMAGE_ID is NOT set`, the PodVM image creation job has not completed yet. Check the job status:

```bash
oc logs -n openshift-sandboxed-containers-operator -l job-name=osc-podvm-image-creation --tail=20
```

Once the image is ready, the `AZURE_IMAGE_ID` field in the `peer-pods-cm` ConfigMap will be populated automatically. Restart the DaemonSet:

```bash
oc rollout restart ds/osc-caa-ds -n openshift-sandboxed-containers-operator
```

### PVC stuck in Pending

Check the CSI driver logs for authentication errors:

```bash
oc describe pvc test-csi-pvc
oc logs -n caa-csi-block -l app=cloud-csi-adaptor -c cloud-csi-adaptor
```

Common causes:
- **Missing Azure credentials** — Ensure the `csi-azure-creds` secret is created and injected as env vars
- **Missing permissions** — Ensure the Service Principal has `Contributor` role on the resource group

### Pod stuck in Pending with "Insufficient kata.peerpods.io/vm"

Restart the osc-caa-ds DaemonSet to re-register extended resources:

```bash
oc rollout restart ds/osc-caa-ds -n openshift-sandboxed-containers-operator
```

### PodVM image creation job failing

The Operator creates a storage account and uses private endpoints for VHD upload. If the job pods repeatedly fail with connection timeouts, check the pod logs:

```bash
oc logs -n openshift-sandboxed-containers-operator -l job-name=osc-podvm-image-creation --tail=30
```

The Operator will retry the job automatically. If failures persist, verify:
- The Service Principal has sufficient permissions in the resource group
- The peerpod subnet has a NAT gateway for outbound internet access
- Network Security Groups allow VNet-to-VNet traffic
