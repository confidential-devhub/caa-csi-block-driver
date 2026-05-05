# Using Cloud CSI Adaptor with CAA on Azure AKS

This guide walks through setting up the Cloud CSI Adaptor with Cloud API Adaptor (CAA) on an Azure Kubernetes Service (AKS) cluster to provision Azure Managed Disks for Peer Pod workloads.

## Prerequisites

- Azure CLI (`az`) installed and logged in
- `kubectl` installed
- `helm` installed
- An Azure subscription with permissions to create AKS clusters and Managed Disks

## 1. Set Up Environment Variables

```bash
export RESOURCE_GROUP="caa-csi-demo"
export CLUSTER_NAME="caa-csi-cluster"
export LOCATION="eastus"
export SUBSCRIPTION_ID=$(az account show --query id -o tsv)
```

## 2. Create an AKS Cluster

```bash
az group create --name $RESOURCE_GROUP --location $LOCATION

az aks create \
  --resource-group $RESOURCE_GROUP \
  --name $CLUSTER_NAME \
  --node-count 1 \
  --node-vm-size Standard_D4s_v5 \
  --enable-oidc-issuer \
  --enable-workload-identity \
  --generate-ssh-keys

az aks get-credentials --resource-group $RESOURCE_GROUP --name $CLUSTER_NAME
```

Verify the cluster is running:

```bash
kubectl get nodes
```

## 3. Install cert-manager

CAA requires cert-manager for webhook certificates:

```bash
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml
kubectl wait --for=condition=Available deployment --all -n cert-manager --timeout=120s
```

## 4. Deploy CAA

Install CAA using the upstream Helm chart. Replace `<CAA_VERSION>` with the desired release (e.g., `v0.19.0`):

```bash
export CAA_VERSION="v0.19.0"

helm install cloud-api-adaptor \
  oci://ghcr.io/confidential-containers/cloud-api-adaptor/helm-charts/cloud-api-adaptor \
  --version $CAA_VERSION \
  --namespace confidential-containers-system \
  --create-namespace \
  --set installCRDs=true
```

Verify CAA is running:

```bash
kubectl get pods -n confidential-containers-system
```

The `cloud-api-adaptor-daemonset` pod should be in `Running` state. Verify the `kata-remote` RuntimeClass exists:

```bash
kubectl get runtimeclass kata-remote
```

## 5. Set Up Workload Identity for the CSI Driver

The CSI driver uses `DefaultAzureCredential` which supports Azure Workload Identity for secure, secretless authentication.

### Create a Managed Identity

```bash
export IDENTITY_NAME="caa-csi-identity"

az identity create \
  --name $IDENTITY_NAME \
  --resource-group $RESOURCE_GROUP \
  --location $LOCATION

export IDENTITY_CLIENT_ID=$(az identity show --name $IDENTITY_NAME --resource-group $RESOURCE_GROUP --query clientId -o tsv)
export IDENTITY_OBJECT_ID=$(az identity show --name $IDENTITY_NAME --resource-group $RESOURCE_GROUP --query principalId -o tsv)
```

### Assign Disk Contributor role

```bash
az role assignment create \
  --assignee-object-id $IDENTITY_OBJECT_ID \
  --assignee-principal-type ServicePrincipal \
  --role "Contributor" \
  --scope /subscriptions/$SUBSCRIPTION_ID/resourceGroups/$RESOURCE_GROUP
```

### Create federated credential

```bash
export AKS_OIDC_ISSUER=$(az aks show --name $CLUSTER_NAME --resource-group $RESOURCE_GROUP --query "oidcIssuerProfile.issuerUrl" -o tsv)

az identity federated-credential create \
  --name caa-csi-federated \
  --identity-name $IDENTITY_NAME \
  --resource-group $RESOURCE_GROUP \
  --issuer $AKS_OIDC_ISSUER \
  --subject system:serviceaccount:caa-csi-block:caa-csi-provisioner \
  --audiences api://AzureADTokenExchange
```

### Annotate the CSI driver ServiceAccount

After deploying the CSI driver (next step), annotate the ServiceAccount:

```bash
kubectl annotate serviceaccount caa-csi-provisioner \
  -n caa-csi-block \
  azure.workload.identity/client-id=$IDENTITY_CLIENT_ID
```

## 6. Deploy the CSI Driver

```bash
helm install caa-csi-block-driver ./charts/caa-csi-block-driver \
  --set provider=azure \
  --set azure.subscriptionId=$SUBSCRIPTION_ID \
  --set azure.resourceGroup=$RESOURCE_GROUP \
  --set azure.location=$LOCATION
```

Annotate the ServiceAccount for Workload Identity:

```bash
kubectl annotate serviceaccount caa-csi-provisioner \
  -n caa-csi-block \
  azure.workload.identity/client-id=$IDENTITY_CLIENT_ID
```

Add the Workload Identity label to the DaemonSet pods:

```bash
kubectl patch daemonset caa-csi-block-driver -n caa-csi-block \
  --type merge \
  -p '{"spec":{"template":{"metadata":{"labels":{"azure.workload.identity/use":"true"}}}}}'
```

Verify the CSI driver pod is running:

```bash
kubectl get pods -n caa-csi-block
```

## 7. Test with a Peer Pod Workload

### Create a PVC

```yaml
# test-pvc.yaml
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
kubectl apply -f test-pvc.yaml
```

Verify the PVC is bound:

```bash
kubectl get pvc test-csi-pvc
```

### Create a test pod with kata-remote runtime

```yaml
# test-pod.yaml
apiVersion: v1
kind: Pod
metadata:
  name: test-csi-pod
spec:
  runtimeClassName: kata-remote
  containers:
    - name: app
      image: busybox
      command: ["sh", "-c", "echo 'Hello from Peer Pod CSI volume!' > /data/test.txt && cat /data/test.txt && sleep 3600"]
      volumeMounts:
        - name: csi-vol
          mountPath: /data
  volumes:
    - name: csi-vol
      persistentVolumeClaim:
        claimName: test-csi-pvc
```

```bash
kubectl apply -f test-pod.yaml
```

### Verify

Wait for the pod to be running:

```bash
kubectl wait --for=condition=Ready pod/test-csi-pod --timeout=300s
```

Check the logs:

```bash
kubectl logs test-csi-pod
# Expected output: Hello from Peer Pod CSI volume!
```

Verify the Azure Managed Disk was created:

```bash
az disk list --resource-group $RESOURCE_GROUP --query "[?tags.\"caa-csi-volume-id\"].[name,diskSizeGb,diskState]" -o table
```

## 8. Cleanup

```bash
kubectl delete pod test-csi-pod
kubectl delete pvc test-csi-pvc
helm uninstall caa-csi-block-driver    # if installed via Helm
az aks delete --name $CLUSTER_NAME --resource-group $RESOURCE_GROUP --yes --no-wait
az group delete --name $RESOURCE_GROUP --yes --no-wait
```

## Troubleshooting

### CSI driver pod in CrashLoopBackOff

Check the logs:

```bash
kubectl logs -n caa-csi-block -l app=caa-csi-block-driver -c caa-csi-block-driver
```

Common causes:
- **Workload Identity not configured** — Ensure the ServiceAccount annotation and pod label are set correctly
- **Missing permissions** — Ensure the Managed Identity has `Contributor` role on the resource group

### PVC stuck in Pending

Check the CSI driver logs and events:

```bash
kubectl describe pvc test-csi-pvc
kubectl logs -n caa-csi-block -l app=caa-csi-block-driver -c caa-csi-block-driver
```

### Pod stuck in Pending with "Insufficient kata.peerpods.io/vm"

Restart the CAA daemonset to re-register extended resources:

```bash
kubectl rollout restart daemonset -n confidential-containers-system cloud-api-adaptor-daemonset
```
