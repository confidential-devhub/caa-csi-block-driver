// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v5"

	provider "github.com/confidential-devhub/caa-csi-block-driver/pkg/provider"
)

var logger = log.New(log.Writer(), "[caa-csi/azure] ", log.LstdFlags|log.Lmsgprefix)

func init() {
	provider.RegisterProvider("azure", func(params map[string]string) (provider.BlockVolumeProvider, error) {
		return NewAzureProvider(params)
	})
}

const (
	defaultDiskSKU = "StandardSSD_LRS"
	volumeTagKey   = "caa-csi-volume-id"
	waitTimeout    = 5 * time.Minute
	pollInterval   = 5 * time.Second
)

type Config struct {
	SubscriptionID string
	ResourceGroup  string
	Location       string
	DiskSKU        string
	DiskIOPS       int64
	DiskMBps       int64
	DiskEncSetID   string
}

type AzureProvider struct {
	disksClient *armcompute.DisksClient
	cred        *azidentity.DefaultAzureCredential
	config      Config
}

func NewAzureProvider(params map[string]string) (*AzureProvider, error) {
	subscriptionID := params["azureSubscriptionId"]
	if subscriptionID == "" {
		return nil, fmt.Errorf("azureSubscriptionId is required for azure provider")
	}

	resourceGroup := params["azureResourceGroup"]
	if resourceGroup == "" {
		return nil, fmt.Errorf("azureResourceGroup is required for azure provider")
	}

	location := params["azureLocation"]
	if location == "" {
		return nil, fmt.Errorf("azureLocation is required for azure provider")
	}

	diskSKU := params["azureDiskSku"]
	if diskSKU == "" {
		diskSKU = defaultDiskSKU
	}

	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create Azure credential: %w", err)
	}

	disksClient, err := armcompute.NewDisksClient(subscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create Azure disks client: %w", err)
	}

	var diskIOPS int64
	if v := params["azureDiskIops"]; v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			diskIOPS = n
		}
	}
	var diskMBps int64
	if v := params["azureDiskMbps"]; v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			diskMBps = n
		}
	}

	return &AzureProvider{
		disksClient: disksClient,
		cred:        cred,
		config: Config{
			SubscriptionID: subscriptionID,
			ResourceGroup:  resourceGroup,
			Location:       location,
			DiskSKU:        diskSKU,
			DiskIOPS:       diskIOPS,
			DiskMBps:       diskMBps,
			DiskEncSetID:   params["azureDiskEncryptionSetId"],
		},
	}, nil
}

func (p *AzureProvider) diskName(volumeID string) string {
	return "csi-vol-" + volumeID
}

func (p *AzureProvider) diskResourceID(name string) string {
	return fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/disks/%s",
		p.config.SubscriptionID, p.config.ResourceGroup, name)
}

func (p *AzureProvider) CreateVolume(volumeID string, sizeBytes int64) (*provider.VolumeInfo, error) {
	ctx := context.TODO()
	name := p.diskName(volumeID)

	exists, err := p.VolumeExists(volumeID)
	if err != nil {
		return nil, err
	}
	if exists {
		logger.Printf("Volume %s already exists, reusing", volumeID)
		return p.GetVolumeInfo(volumeID)
	}

	sizeGiB := int32(sizeBytes / (1024 * 1024 * 1024))
	if sizeGiB == 0 {
		sizeGiB = 1
	}

	logger.Printf("Creating Azure Managed Disk %s (%d GiB, sku=%s, location=%s)",
		name, sizeGiB, p.config.DiskSKU, p.config.Location)

	diskProps := &armcompute.DiskProperties{
		CreationData: &armcompute.CreationData{
			CreateOption: to.Ptr(armcompute.DiskCreateOptionEmpty),
		},
		DiskSizeGB: to.Ptr(sizeGiB),
	}
	if p.config.DiskIOPS > 0 {
		diskProps.DiskIOPSReadWrite = to.Ptr(p.config.DiskIOPS)
	}
	if p.config.DiskMBps > 0 {
		diskProps.DiskMBpsReadWrite = to.Ptr(p.config.DiskMBps)
	}
	if p.config.DiskEncSetID != "" {
		diskProps.Encryption = &armcompute.Encryption{
			DiskEncryptionSetID: to.Ptr(p.config.DiskEncSetID),
			Type:                to.Ptr(armcompute.EncryptionTypeEncryptionAtRestWithCustomerKey),
		}
	}

	disk := armcompute.Disk{
		Location:   to.Ptr(p.config.Location),
		SKU: &armcompute.DiskSKU{
			Name: to.Ptr(armcompute.DiskStorageAccountTypes(p.config.DiskSKU)),
		},
		Properties: diskProps,
		Tags: map[string]*string{
			volumeTagKey: to.Ptr(volumeID),
		},
	}

	poller, err := p.disksClient.BeginCreateOrUpdate(ctx, p.config.ResourceGroup, name, disk, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin creating disk %s: %w", name, err)
	}

	result, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create disk %s: %w", name, err)
	}

	diskID := *result.ID
	logger.Printf("Created Azure Managed Disk %s (id=%s)", name, diskID)

	return &provider.VolumeInfo{
		VolumeID:  volumeID,
		Path:      diskID,
		SizeBytes: sizeBytes,
		Provider:  "azure",
		Metadata: map[string]string{
			"cloud-volume-path": diskID,
			"cloud-provider":    "azure",
			"azure-disk-name":   name,
			"azure-resource-id": diskID,
		},
	}, nil
}

func (p *AzureProvider) DeleteVolume(volumeID string) error {
	ctx := context.TODO()
	name := p.diskName(volumeID)

	logger.Printf("Deleting Azure Managed Disk %s", name)

	poller, err := p.disksClient.BeginDelete(ctx, p.config.ResourceGroup, name, nil)
	if err != nil {
		if strings.Contains(err.Error(), "ResourceNotFound") || strings.Contains(err.Error(), "NotFound") {
			logger.Printf("Disk %s not found, nothing to delete", name)
			return nil
		}
		return fmt.Errorf("failed to begin deleting disk %s: %w", name, err)
	}

	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("failed to delete disk %s: %w", name, err)
	}

	logger.Printf("Deleted Azure Managed Disk %s", name)
	return nil
}

func (p *AzureProvider) GetVolumeInfo(volumeID string) (*provider.VolumeInfo, error) {
	ctx := context.TODO()
	name := p.diskName(volumeID)

	result, err := p.disksClient.Get(ctx, p.config.ResourceGroup, name, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get disk %s: %w", name, err)
	}

	diskID := *result.ID
	var sizeBytes int64
	if result.Properties != nil && result.Properties.DiskSizeGB != nil {
		sizeBytes = int64(*result.Properties.DiskSizeGB) * 1024 * 1024 * 1024
	}

	return &provider.VolumeInfo{
		VolumeID:  volumeID,
		Path:      diskID,
		SizeBytes: sizeBytes,
		Provider:  "azure",
		Metadata: map[string]string{
			"cloud-volume-path": diskID,
			"cloud-provider":    "azure",
			"azure-disk-name":   name,
			"azure-resource-id": diskID,
		},
	}, nil
}

func (p *AzureProvider) VolumeExists(volumeID string) (bool, error) {
	ctx := context.TODO()
	name := p.diskName(volumeID)

	_, err := p.disksClient.Get(ctx, p.config.ResourceGroup, name, nil)
	if err != nil {
		if strings.Contains(err.Error(), "ResourceNotFound") || strings.Contains(err.Error(), "NotFound") {
			return false, nil
		}
		return false, fmt.Errorf("failed to check disk %s: %w", name, err)
	}
	return true, nil
}

// ListManagedVolumes returns all Azure Managed Disks in the resource group
// that are tagged with our CSI tag.
func (p *AzureProvider) ListManagedVolumes() ([]*provider.VolumeInfo, error) {
	ctx := context.TODO()

	pager := p.disksClient.NewListByResourceGroupPager(p.config.ResourceGroup, nil)
	var vols []*provider.VolumeInfo

	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing disks for recovery: %w", err)
		}
		for _, d := range page.Value {
			if d.Tags == nil {
				continue
			}
			tagVal, ok := d.Tags[volumeTagKey]
			if !ok || tagVal == nil {
				continue
			}
			csiVolumeID := *tagVal
			diskID := ""
			if d.ID != nil {
				diskID = *d.ID
			}
			var sizeBytes int64
			if d.Properties != nil && d.Properties.DiskSizeGB != nil {
				sizeBytes = int64(*d.Properties.DiskSizeGB) * 1024 * 1024 * 1024
			}
			vols = append(vols, &provider.VolumeInfo{
				VolumeID:  csiVolumeID,
				Path:      diskID,
				SizeBytes: sizeBytes,
				Provider:  "azure",
				Metadata: map[string]string{
					"cloud-volume-path": diskID,
					"cloud-provider":    "azure",
					"azure-disk-name":   p.diskName(csiVolumeID),
					"azure-resource-id": diskID,
				},
			})
		}
	}
	return vols, nil
}

// ExpandVolume resizes an Azure Managed Disk to newSizeBytes.
func (p *AzureProvider) ExpandVolume(volumeID string, newSizeBytes int64) error {
	ctx := context.TODO()
	name := p.diskName(volumeID)

	newSizeGiB := int32(newSizeBytes / (1024 * 1024 * 1024))
	if newSizeGiB == 0 {
		newSizeGiB = 1
	}

	logger.Printf("Expanding Azure Managed Disk %s to %d GiB", name, newSizeGiB)

	disk := armcompute.DiskUpdate{
		Properties: &armcompute.DiskUpdateProperties{
			DiskSizeGB: to.Ptr(newSizeGiB),
		},
	}

	poller, err := p.disksClient.BeginUpdate(ctx, p.config.ResourceGroup, name, disk, nil)
	if err != nil {
		return fmt.Errorf("failed to begin expanding disk %s: %w", name, err)
	}
	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("failed to expand disk %s: %w", name, err)
	}

	logger.Printf("Azure Managed Disk %s expanded to %d GiB", name, newSizeGiB)
	return nil
}

// CreateVolumeFromSnapshot creates a new Azure Managed Disk from an existing snapshot.
func (p *AzureProvider) CreateVolumeFromSnapshot(volumeID, snapshotID string, sizeBytes int64) (*provider.VolumeInfo, error) {
	ctx := context.TODO()
	name := p.diskName(volumeID)
	snapName := "csi-snap-" + snapshotID

	sizeGiB := int32(sizeBytes / (1024 * 1024 * 1024))
	if sizeGiB == 0 {
		sizeGiB = 1
	}

	snapResourceID := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/snapshots/%s",
		p.config.SubscriptionID, p.config.ResourceGroup, snapName)

	logger.Printf("Creating Azure Managed Disk %s from snapshot %s (%d GiB)", name, snapName, sizeGiB)

	disk := armcompute.Disk{
		Location: to.Ptr(p.config.Location),
		SKU: &armcompute.DiskSKU{
			Name: to.Ptr(armcompute.DiskStorageAccountTypes(p.config.DiskSKU)),
		},
		Properties: &armcompute.DiskProperties{
			CreationData: &armcompute.CreationData{
				CreateOption:     to.Ptr(armcompute.DiskCreateOptionCopy),
				SourceResourceID: to.Ptr(snapResourceID),
			},
			DiskSizeGB: to.Ptr(sizeGiB),
		},
		Tags: map[string]*string{
			volumeTagKey:    to.Ptr(volumeID),
			"source-snapshot": to.Ptr(snapshotID),
		},
	}

	poller, err := p.disksClient.BeginCreateOrUpdate(ctx, p.config.ResourceGroup, name, disk, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin creating disk from snapshot %s: %w", snapName, err)
	}
	result, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create disk from snapshot %s: %w", snapName, err)
	}

	diskID := *result.ID
	return &provider.VolumeInfo{
		VolumeID:  volumeID,
		Path:      diskID,
		SizeBytes: sizeBytes,
		Provider:  "azure",
		Metadata: map[string]string{
			"cloud-volume-path": diskID,
			"cloud-provider":    "azure",
			"azure-disk-name":   name,
			"source-snapshot":   snapshotID,
		},
	}, nil
}

// CreateVolumeFromVolume creates a new Azure Managed Disk by copying
// directly from the source disk.
func (p *AzureProvider) CreateVolumeFromVolume(volumeID, sourceVolumeID string, sizeBytes int64) (*provider.VolumeInfo, error) {
	ctx := context.TODO()
	name := p.diskName(volumeID)
	sourceName := p.diskName(sourceVolumeID)

	sizeGiB := int32(sizeBytes / (1024 * 1024 * 1024))
	if sizeGiB == 0 {
		sizeGiB = 1
	}

	sourceResourceID := p.diskResourceID(sourceName)
	logger.Printf("Cloning Azure Managed Disk %s → %s (%d GiB)", sourceName, name, sizeGiB)

	disk := armcompute.Disk{
		Location: to.Ptr(p.config.Location),
		SKU: &armcompute.DiskSKU{
			Name: to.Ptr(armcompute.DiskStorageAccountTypes(p.config.DiskSKU)),
		},
		Properties: &armcompute.DiskProperties{
			CreationData: &armcompute.CreationData{
				CreateOption:     to.Ptr(armcompute.DiskCreateOptionCopy),
				SourceResourceID: to.Ptr(sourceResourceID),
			},
			DiskSizeGB: to.Ptr(sizeGiB),
		},
		Tags: map[string]*string{
			volumeTagKey:     to.Ptr(volumeID),
			"source-volume":  to.Ptr(sourceVolumeID),
		},
	}

	poller, err := p.disksClient.BeginCreateOrUpdate(ctx, p.config.ResourceGroup, name, disk, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin cloning disk %s: %w", sourceName, err)
	}
	result, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to clone disk %s: %w", sourceName, err)
	}

	diskID := *result.ID
	return &provider.VolumeInfo{
		VolumeID:  volumeID,
		Path:      diskID,
		SizeBytes: sizeBytes,
		Provider:  "azure",
		Metadata: map[string]string{
			"cloud-volume-path": diskID,
			"cloud-provider":    "azure",
			"azure-disk-name":   name,
			"source-volume":     sourceVolumeID,
		},
	}, nil
}

// CreateSnapshot creates an Azure snapshot from the given volume's disk.
func (p *AzureProvider) CreateSnapshot(volumeID, snapshotID string) (*provider.SnapshotInfo, error) {
	ctx := context.TODO()
	diskName := p.diskName(volumeID)

	diskInfo, err := p.disksClient.Get(ctx, p.config.ResourceGroup, diskName, nil)
	if err != nil {
		return nil, fmt.Errorf("cannot find disk %s for snapshot: %w", diskName, err)
	}

	snapshotsClient, err := armcompute.NewSnapshotsClient(p.config.SubscriptionID, p.credential(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create snapshots client: %w", err)
	}

	snapName := "csi-snap-" + snapshotID
	logger.Printf("Creating Azure snapshot %s from disk %s", snapName, diskName)

	snapshot := armcompute.Snapshot{
		Location: to.Ptr(p.config.Location),
		Properties: &armcompute.SnapshotProperties{
			CreationData: &armcompute.CreationData{
				CreateOption:     to.Ptr(armcompute.DiskCreateOptionCopy),
				SourceResourceID: diskInfo.ID,
			},
		},
		Tags: map[string]*string{
			"caa-csi-snapshot-id": to.Ptr(snapshotID),
			volumeTagKey:          to.Ptr(volumeID),
		},
	}

	poller, err := snapshotsClient.BeginCreateOrUpdate(ctx, p.config.ResourceGroup, snapName, snapshot, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin creating snapshot %s: %w", snapName, err)
	}
	result, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create snapshot %s: %w", snapName, err)
	}

	var sizeBytes int64
	if result.Properties != nil && result.Properties.DiskSizeGB != nil {
		sizeBytes = int64(*result.Properties.DiskSizeGB) * 1024 * 1024 * 1024
	}
	var creationTime int64
	if result.Properties != nil && result.Properties.TimeCreated != nil {
		creationTime = result.Properties.TimeCreated.Unix()
	}

	return &provider.SnapshotInfo{
		SnapshotID:     snapshotID,
		SourceVolumeID: volumeID,
		SizeBytes:      sizeBytes,
		CreationTime:   creationTime,
		ReadyToUse:     result.Properties != nil && result.Properties.ProvisioningState != nil && *result.Properties.ProvisioningState == "Succeeded",
	}, nil
}

// DeleteSnapshot deletes an Azure snapshot.
func (p *AzureProvider) DeleteSnapshot(snapshotID string) error {
	ctx := context.TODO()

	snapshotsClient, err := armcompute.NewSnapshotsClient(p.config.SubscriptionID, p.credential(), nil)
	if err != nil {
		return fmt.Errorf("failed to create snapshots client: %w", err)
	}

	snapName := "csi-snap-" + snapshotID
	logger.Printf("Deleting Azure snapshot %s", snapName)

	poller, err := snapshotsClient.BeginDelete(ctx, p.config.ResourceGroup, snapName, nil)
	if err != nil {
		if strings.Contains(err.Error(), "ResourceNotFound") || strings.Contains(err.Error(), "NotFound") {
			logger.Printf("Snapshot %s not found, nothing to delete", snapName)
			return nil
		}
		return fmt.Errorf("failed to begin deleting snapshot %s: %w", snapName, err)
	}
	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("failed to delete snapshot %s: %w", snapName, err)
	}

	logger.Printf("Deleted Azure snapshot %s", snapName)
	return nil
}

// ListSnapshots lists Azure snapshots for the given volume. The ARM
// Snapshots API does not support server-side tag or name-prefix
// filtering, so we iterate the resource group and filter client-side.
// We skip snapshots early based on the "csi-snap-" name prefix to
// minimise allocations and tag parsing.
func (p *AzureProvider) ListSnapshots(volumeID string) ([]*provider.SnapshotInfo, error) {
	ctx := context.TODO()

	snapshotsClient, err := armcompute.NewSnapshotsClient(p.config.SubscriptionID, p.credential(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create snapshots client: %w", err)
	}

	pager := snapshotsClient.NewListByResourceGroupPager(p.config.ResourceGroup, nil)
	var snaps []*provider.SnapshotInfo

	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing snapshots: %w", err)
		}
		for _, s := range page.Value {
			if s.Name == nil || !strings.HasPrefix(*s.Name, "csi-snap-") {
				continue
			}
			if s.Tags == nil {
				continue
			}
			tagVal, ok := s.Tags[volumeTagKey]
			if !ok || tagVal == nil || *tagVal != volumeID {
				continue
			}
			snapID := ""
			if v, ok := s.Tags["caa-csi-snapshot-id"]; ok && v != nil {
				snapID = *v
			}
			var sizeBytes int64
			if s.Properties != nil && s.Properties.DiskSizeGB != nil {
				sizeBytes = int64(*s.Properties.DiskSizeGB) * 1024 * 1024 * 1024
			}
			var creationTime int64
			if s.Properties != nil && s.Properties.TimeCreated != nil {
				creationTime = s.Properties.TimeCreated.Unix()
			}
			snaps = append(snaps, &provider.SnapshotInfo{
				SnapshotID:     snapID,
				SourceVolumeID: volumeID,
				SizeBytes:      sizeBytes,
				CreationTime:   creationTime,
				ReadyToUse:     s.Properties != nil && s.Properties.ProvisioningState != nil && *s.Properties.ProvisioningState == "Succeeded",
			})
		}
	}
	return snaps, nil
}

// credential returns the cached DefaultAzureCredential for sub-clients.
func (p *AzureProvider) credential() *azidentity.DefaultAzureCredential {
	return p.cred
}
