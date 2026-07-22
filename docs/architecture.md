# CAA CSI Block Driver — Architecture & Volume Lifecycle

## Overview

The `caa-csi-block-driver` is a Container Storage Interface (CSI) driver that
provisions cloud block storage (AWS EBS, Azure Managed Disks) for Kata
Containers **Peer Pods**. Unlike traditional CSI drivers that attach disks
directly to the kubelet node, this driver creates cloud disks and passes
metadata through to the Cloud API Adaptor (CAA), which attaches them to the
PodVM at creation time.

```
┌──────────────┐  PVC/PV  ┌──────────────┐  cloud_volumes  ┌─────────────┐
│   kubelet    │────────►│ caa-csi-block │───────────────►│  CAA proxy  │
│              │          │    driver     │   annotation    │             │
└──────────────┘          └──────┬───────┘                 └──────┬──────┘
                                 │                                │
                          CreateVolume()                  VM Create w/ disks
                                 │                                │
                          ┌──────▼───────┐                 ┌──────▼──────┐
                          │  Cloud API   │                 │   PodVM     │
                          │ (EBS / Disk) │                 │ interceptor │
                          └──────────────┘                 └─────────────┘
```

## Volume Lifecycle

### 1. Provisioning (CreateVolume)

1. PVC is created referencing the CSI StorageClass.
2. The `csi-provisioner` sidecar calls `CreateVolume` on the controller server
   → CSI driver creates the cloud volume (EBS / Managed Disk) via the provider
   API and persists a `volumeRecord` to the local store.
3. Kubernetes scheduler selects a node for the pod.
4. kubelet calls `NodeStageVolume` → CSI driver records the device mapping.
5. kubelet calls `NodePublishVolume` → CSI driver writes a `mountInfo.json`
   file under the Kata direct-volumes path so the kata-runtime can find it.

### 2. PodVM Attachment

1. CAA proxy reads `mountInfo.json` and constructs the `cloud_volumes`
   annotation with volume metadata (LUN, mount point, fs type, disk ID).
2. For **Azure**: disks are attached as data disks during VM creation (in the
   ARM template's `dataDisks` array, each with a LUN index).
3. For **AWS**: disks are attached via `AttachVolume` API calls after the
   instance reaches `running` state, in parallel via `errgroup`.

### 3. PodVM Mount (Interceptor)

Inside the PodVM, the `agent-protocol-forwarder` interceptor:

1. Parses the `cloud_volumes` annotation from `CreateContainerRequest`.
2. Detects the cloud provider (Azure via `/sys/class/dmi/id/sys_vendor`,
   AWS via `/sys/devices/virtual/dmi/id/board_asset_tag`).
3. Finds the data disk device:
   - **Azure**: scans `/dev/disk/azure/data/by-lun/<LUN>` symlinks (created
     by custom udev rules in the PodVM image). Falls back to sysfs HCTL scan.
   - **AWS**: scans `/dev/disk/by-path/*-lun-<LUN>` symlinks. Falls back to
     sysfs HCTL scan.
   - **Libvirt**: scans virtio devices via sysfs HCTL.
4. Formats the disk if needed (`mkfs.ext4` / `mkfs.xfs`), checking for
   existing filesystems to preserve data.
5. Mounts the disk at the requested mount point.

### 4. Cleanup (DeleteVolume)

1. Pod deletion triggers `NodeUnpublishVolume` → cleans up `mountInfo.json`
   and the target path.
2. `NodeUnstageVolume` → removes the device mapping from memory.
3. PV deletion triggers `DeleteVolume` (controller) → deletes the cloud disk
   via the provider API.

## Orphaned Disk Handling

### Problem
If a PodVM crashes or is force-deleted, its data disks may remain in
"Attached" state on the stale VM, blocking reuse.

### Solution
- **AWS**: `attachEBSVolumes` detects `VolumeInUse` errors and automatically
  calls `forceDetachVolume` (with `Force=true`) before retrying the attach.
- **Azure**: `CreateInstance` calls `forceDetachStaleDisks` before VM creation.
  This inspects each disk's state; if it is `Attached`/`Reserved`, it reads
  the `ManagedBy` VM, removes the disk from that VM's storage profile via
  `BeginCreateOrUpdate`, and waits for completion.

## Volume Store Recovery

The CSI driver persists volume metadata as JSON files under
`/var/lib/caa-csi-block/volumes/` (hostPath). Controller operations such as
`DeleteVolume` and `ControllerExpandVolume` need those records (or equivalent
bootstrap params) to talk to the cloud API.

If the CSI pod moves to another node, or the store directory is wiped, the
controller recovers state as follows:

1. **Bootstrap params** are resolved in order:
   1. Params from any surviving local volume record
   2. Params from `_manifest.json` in the volume store (kept even after the
      last volume is deleted)
   3. JSON file from `CSI_BOOTSTRAP_PARAMS_FILE` (recommended ConfigMap mount)
   4. Environment variables (`CSI_CLOUD_PROVIDER`, `CSI_AWS_REGION` /
      `AWS_REGION`, `CSI_AZURE_SUBSCRIPTION_ID`, …)
2. `ListManagedVolumes()` lists cloud disks tagged with `caa-csi-volume-id`.
3. Missing local volume records are rebuilt from the cloud response.
4. If a volume is still missing at `DeleteVolume` time, the driver deletes the
   cloud disk using bootstrap params instead of silently succeeding (which
   previously leaked disks).

### Recommended empty-store bootstrap

Apply the provider ConfigMap, then the DaemonSet (both already wire
`CSI_BOOTSTRAP_PARAMS_FILE`):

```bash
# AWS
kubectl apply -f deploy/bootstrap-params-aws.yaml
kubectl apply -f deploy/daemonset-aws.yaml

# Azure — edit subscription/resource group in the ConfigMap first
kubectl apply -f deploy/bootstrap-params-azure.yaml
kubectl apply -f deploy/daemonset-azure.yaml
```

Helm installs the same ConfigMap automatically from `values.yaml`
(`aws.*` / `azure.*` / `libvirt.*`).

For AWS, `awsRegion` (or `AWS_REGION`) is enough for list/delete/recovery;
`awsAvailabilityZone` is only required when creating new volumes.



## Pre-Creation Validation (Azure)

Before creating the PodVM, the Azure provider performs:

1. **Disk count validation** (`validateDiskCount`): queries VM size
   capabilities to ensure the requested number of data disks does not exceed
   the VM's `MaxDataDiskCount`.
2. **Per-disk validation** (`validateDiskZones`): for each volume, verifies:
   - The disk exists (returns `NotFound` error with disk name and RG).
   - The disk is not already attached to another VM.
   - The disk's region matches the PodVM's target region.

## Volume Expansion

Online volume expansion is supported:

1. `ControllerExpandVolume` → calls `provider.ExpandVolume()`:
   - **AWS**: `ec2.ModifyVolume` to resize the EBS volume.
   - **Azure**: `disksClient.BeginUpdate` to resize the Managed Disk.
2. `NodeExpandVolume` → detects filesystem type and runs:
   - ext2/ext3/ext4: `resize2fs`
   - xfs: `xfs_growfs`

## Snapshots

Snapshot lifecycle is supported via the `VolumeSnapshotter` interface:

- **AWS**: `CreateSnapshot` → `ec2.CreateSnapshot` with tags;
  `DeleteSnapshot` → `ec2.DeleteSnapshot`; `ListSnapshots` →
  `ec2.DescribeSnapshots` filtered by volume tag.
- **Azure**: `CreateSnapshot` → `snapshotsClient.BeginCreateOrUpdate` with
  `DiskCreateOptionCopy`; `DeleteSnapshot` →
  `snapshotsClient.BeginDelete`; `ListSnapshots` →
  `snapshotsClient.NewListByResourceGroupPager` filtered by tag.

---

## Troubleshooting Guide

### Pod stuck in `ContainerCreating`

| Symptom | Likely Cause | Fix |
|---------|-------------|-----|
| `CreateContainerRequest timed out` | PodVM cannot reach container registry (no outbound internet) | Add NAT Gateway to subnet |
| `no data disk found for LUN N` | udev rules missing in PodVM image; disk not attached | Verify `66-azure-storage.rules` in image; check CAA logs for attach errors |
| `corrupt cloud_volumes annotation` | `mountInfo.json` malformed or CSI driver not deployed | Check CSI driver pods; verify PVC is bound |
| `device already mounted or mount point busy` | Stale mount from previous attempt | Delete orphaned PodVM; CAA will retry |

### PVC stuck in `Pending`

| Symptom | Likely Cause | Fix |
|---------|-------------|-----|
| `failed to create provider` | StorageClass parameters missing `cloudProvider` | Add `cloudProvider: aws` or `cloudProvider: azure` to StorageClass |
| `azureSubscriptionId is required` | Missing Azure parameters in StorageClass | Add required Azure parameters |
| Volume quota exceeded | Cloud account disk limit reached | Request quota increase or delete unused disks |

### Data loss after pod reschedule

| Symptom | Likely Cause | Fix |
|---------|-------------|-----|
| Fresh filesystem on reschedule | Interceptor reformatted the disk | Ensure `formatAndMount` checks for existing FS (fixed in current code) |
| Volume not found / PVC delete leaks disk | CSI driver pod rescheduled, lost local state without bootstrap params | Set `CSI_CLOUD_PROVIDER` + provider params or `CSI_BOOTSTRAP_PARAMS_FILE`; volume store recovery rebuilds from cloud tags on startup |

### Cross-zone errors

| Symptom | Likely Cause | Fix |
|---------|-------------|-----|
| `disk is in region X but PodVM targets region Y` | Disk created in different region than node | Align StorageClass AZ/region with the node; topology-aware provisioning tracked in #34 |
| `ConflictingUserInput` on Azure VM create | Disk still attached to stale VM | Orphan detach logic handles this automatically; if it persists, manually delete stale VM |
