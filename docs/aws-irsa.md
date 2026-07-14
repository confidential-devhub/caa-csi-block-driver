# AWS IRSA Setup Guide for CAA CSI Block Driver

This guide explains how to use IAM Roles for Service Accounts (IRSA) with
the CAA CSI Block Driver on Amazon EKS. IRSA eliminates the need for static
AWS credentials stored in Kubernetes secrets.

## How it works

When `aws.irsa.enabled=true` and `aws.irsa.roleArn` is set, the Helm chart
annotates the CSI driver's ServiceAccount with `eks.amazonaws.com/role-arn`.
EKS's mutating admission webhook then injects a projected token and
environment variables (`AWS_WEB_IDENTITY_TOKEN_FILE`, `AWS_ROLE_ARN`) into
the driver pod. The AWS SDK picks these up automatically via its default
credential chain — no code changes needed.

> **Note:** This guide is for Amazon EKS only. For self-managed Kubernetes
> clusters, you must manually configure projected service account tokens
> and set the environment variables yourself.

## Prerequisites

- EKS cluster (v1.13+)
- `aws` CLI v2, `kubectl`, `eksctl`
- IAM permissions to create roles and OIDC providers

## Step 1: Enable OIDC provider for your EKS cluster

```bash
export CLUSTER_NAME="my-eks-cluster"
export AWS_REGION="us-east-2"

# Check if OIDC issuer exists
aws eks describe-cluster \
  --name ${CLUSTER_NAME} \
  --region ${AWS_REGION} \
  --query "cluster.identity.oidc.issuer" \
  --output text

# Create OIDC provider if not already registered
eksctl utils associate-iam-oidc-provider \
  --cluster ${CLUSTER_NAME} \
  --region ${AWS_REGION} \
  --approve
```

## Step 2: Get account ID and OIDC provider

```bash
export ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)

export OIDC_PROVIDER=$(aws eks describe-cluster \
  --name ${CLUSTER_NAME} \
  --region ${AWS_REGION} \
  --query "cluster.identity.oidc.issuer" \
  --output text | sed 's|https://||')
```

## Step 3: Create IAM role with EBS permissions

### Define variables

```bash
export CSI_NAMESPACE="caa-csi-block"
export CSI_SERVICE_ACCOUNT="<your-helm-release>-caa-csi-block-driver"
export CSI_ROLE_NAME="CAA-CSI-Block-IRSA-Role"
```

> **Tip:** The ServiceAccount name defaults to `<release>-caa-csi-block-driver`.
> If you set `serviceAccount.name` in your Helm values, use that instead.

### Create trust policy

```bash
cat > /tmp/csi-trust-policy.json <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "Federated": "arn:aws:iam::${ACCOUNT_ID}:oidc-provider/${OIDC_PROVIDER}"
      },
      "Action": "sts:AssumeRoleWithWebIdentity",
      "Condition": {
        "StringEquals": {
          "${OIDC_PROVIDER}:sub": "system:serviceaccount:${CSI_NAMESPACE}:${CSI_SERVICE_ACCOUNT}",
          "${OIDC_PROVIDER}:aud": "sts.amazonaws.com"
        }
      }
    }
  ]
}
EOF
```

### Create the role

```bash
aws iam create-role \
  --role-name ${CSI_ROLE_NAME} \
  --assume-role-policy-document file:///tmp/csi-trust-policy.json \
  --description "IRSA role for CAA CSI Block Driver"
```

### Attach EBS permissions

**Option A: AWS managed policy (simpler)**

```bash
aws iam attach-role-policy \
  --role-name ${CSI_ROLE_NAME} \
  --policy-arn arn:aws:iam::aws:policy/AmazonEC2FullAccess
```

**Option B: Least-privilege custom policy (recommended)**

```bash
cat > /tmp/csi-ebs-policy.json <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "CAACSIBlockVolumeOps",
      "Effect": "Allow",
      "Action": [
        "ec2:CreateVolume",
        "ec2:DeleteVolume",
        "ec2:DescribeVolumes",
        "ec2:ModifyVolume",
        "ec2:CreateSnapshot",
        "ec2:DeleteSnapshot",
        "ec2:DescribeSnapshots",
        "ec2:CreateTags"
      ],
      "Resource": "*"
    }
  ]
}
EOF

aws iam create-policy \
  --policy-name CAA-CSI-Block-EBS-Policy \
  --policy-document file:///tmp/csi-ebs-policy.json

aws iam attach-role-policy \
  --role-name ${CSI_ROLE_NAME} \
  --policy-arn arn:aws:iam::${ACCOUNT_ID}:policy/CAA-CSI-Block-EBS-Policy
```

### Get the role ARN

```bash
export CSI_ROLE_ARN=$(aws iam get-role \
  --role-name ${CSI_ROLE_NAME} \
  --query 'Role.Arn' \
  --output text)

echo "Role ARN: ${CSI_ROLE_ARN}"
```

## Step 4: Deploy with IRSA

```bash
helm install caa-csi charts/caa-csi-block-driver \
  --set provider=aws \
  --set aws.region=us-east-2 \
  --set aws.availabilityZone=us-east-2c \
  --set aws.irsa.enabled=true \
  --set aws.irsa.roleArn=${CSI_ROLE_ARN}
```

Do **not** set `aws.staticCredentials.enabled=true` — IRSA and static
credentials are mutually exclusive (the chart will fail if both are set).

## Step 5: Verify IRSA is working

### Check ServiceAccount annotation

```bash
kubectl get serviceaccount -n caa-csi-block -o yaml | grep role-arn
```

Expected:

```
eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/CAA-CSI-Block-IRSA-Role
```

### Check pod environment

```bash
CSI_POD=$(kubectl get pods -n caa-csi-block -l app.kubernetes.io/name=caa-csi-block-driver \
  -o jsonpath='{.items[0].metadata.name}')

kubectl exec -n caa-csi-block ${CSI_POD} -c caa-csi-block-driver -- env | grep AWS
```

Expected output should include:

```
AWS_WEB_IDENTITY_TOKEN_FILE=/var/run/secrets/eks.amazonaws.com/serviceaccount/token
AWS_ROLE_ARN=arn:aws:iam::123456789012:role/CAA-CSI-Block-IRSA-Role
```

### Test volume creation

Create a PVC and a pod to verify the driver can provision EBS volumes:

```bash
kubectl apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: irsa-test-pvc
  namespace: default
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: <your-storage-class>
  resources:
    requests:
      storage: 1Gi
EOF
```

If the PVC binds successfully, IRSA is working.

## Migrating from static credentials

If you already have the driver deployed with static credentials:

```bash
# Upgrade with IRSA
helm upgrade caa-csi charts/caa-csi-block-driver \
  --set provider=aws \
  --set aws.region=us-east-2 \
  --set aws.availabilityZone=us-east-2c \
  --set aws.irsa.enabled=true \
  --set aws.irsa.roleArn=${CSI_ROLE_ARN} \
  --set aws.staticCredentials.enabled=false

# After verifying IRSA works, delete the static credentials secret
kubectl delete secret caa-csi-aws-creds -n caa-csi-block
```

## References

- [AWS IRSA documentation](https://docs.aws.amazon.com/eks/latest/userguide/iam-roles-for-service-accounts.html)
- [Upstream CAA IRSA guide](https://github.com/confidential-containers/cloud-api-adaptor/blob/main/src/cloud-api-adaptor/docs/aws-irsa.md)
- [EKS best practices for security](https://aws.github.io/aws-eks-best-practices/security/docs/)
