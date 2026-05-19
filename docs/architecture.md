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

1. Kubernetes scheduler selects a node for the pod.
2. kubelet calls `NodeStageVolume` → CSI driver creates the cloud volume
   (EBS / Managed Disk) and persists a `volumeRecord` to the local store.
3. kubelet calls `NodePublishVolume` → CSI driver writes a `mountInfo.json`
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
`/var/lib/caa-csi-block/volumes/`. If the CSI pod is rescheduled and these
files are lost, the controller attempts cloud-side recovery at startup:

1. Reads any surviving `volumeRecord` to obtain cloud provider parameters.
2. Calls `ListManagedVolumes()` on the provider, which queries for all
   cloud disks tagged with `caa-csi-volume-id`.
3. Rebuilds missing local volume records from the cloud response.

## Topology Awareness

The node server advertises topology segments via `NodeGetInfo`:

```
topology.caa-csi.io/region = <CSI_TOPOLOGY_REGION>
topology.caa-csi.io/zone   = <CSI_TOPOLOGY_ZONE>
```

Set these environment variables on the CSI node DaemonSet. The controller
propagates `AccessibilityRequirements` from `CreateVolume` requests to the
volume response, enabling the Kubernetes scheduler to avoid cross-zone
volume-to-node mismatches.

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
| Volume not found | CSI driver pod rescheduled, lost local state | Volume store recovery will rebuild from cloud tags on next startup |

### Cross-zone errors

| Symptom | Likely Cause | Fix |
|---------|-------------|-----|
| `disk is in region X but PodVM targets region Y` | Disk created in different region than node | Set `CSI_TOPOLOGY_REGION` / `CSI_TOPOLOGY_ZONE` env vars; use topology-aware StorageClass |
| `ConflictingUserInput` on Azure VM create | Disk still attached to stale VM | Orphan detach logic handles this automatically; if it persists, manually delete stale VM |
