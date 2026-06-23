// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"fmt"
	"log"
	"regexp"
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

// Compile-time interface checks.
var (
	_ provider.BlockVolumeProvider = (*AzureProvider)(nil)
	_ provider.VolumeSnapshotter   = (*AzureProvider)(nil)
)

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
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid azureDiskIops %q: must be a positive integer", v)
		}
		diskIOPS = n
	}
	var diskMBps int64
	if v := params["azureDiskMbps"]; v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid azureDiskMbps %q: must be a positive integer", v)
		}
		diskMBps = n
	}

	if diskIOPS > 0 && diskSKU != "UltraSSD_LRS" && diskSKU != "PremiumV2_LRS" {
		return nil, fmt.Errorf("azureDiskIops is only supported for UltraSSD_LRS and PremiumV2_LRS (got %s)", diskSKU)
	}
	if diskMBps > 0 && diskSKU != "UltraSSD_LRS" && diskSKU != "PremiumV2_LRS" {
		return nil, fmt.Errorf("azureDiskMbps is only supported for UltraSSD_LRS and PremiumV2_LRS (got %s)", diskSKU)
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

var azureDiskNameInvalidChars = regexp.MustCompile(`[^a-zA-Z0-9_.\-]`)

const (
	azureDiskNamePrefix = "csi-vol-"
	azureDiskNameMaxLen = 80
)

func (p *AzureProvider) diskName(volumeID string) string {
	sanitized := azureDiskNameInvalidChars.ReplaceAllString(volumeID, "-")
	name := azureDiskNamePrefix + sanitized
	if len(name) > azureDiskNameMaxLen {
		name = name[:azureDiskNameMaxLen]
	}
	return name
}

func (p *AzureProvider) diskResourceID(name string) string {
	return fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/disks/%s",
		p.config.SubscriptionID, p.config.ResourceGroup, name)
}

func (p *AzureProvider) CreateVolume(volumeID string, sizeBytes int64) (*provider.VolumeInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	name := p.diskName(volumeID)

	exists, err := p.VolumeExists(volumeID)
	if err != nil {
		return nil, err
	}
	if exists {
		logger.Printf("Volume %s already exists, reusing", volumeID)
		return p.GetVolumeInfo(volumeID)
	}

	const gib = 1024 * 1024 * 1024
	sizeGiB := int32((sizeBytes + gib - 1) / gib)
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

	if result.ID == nil {
		return nil, fmt.Errorf("Azure returned nil disk ID for %s", name)
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
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
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

	if result.ID == nil {
		return nil, fmt.Errorf("Azure returned nil disk ID for %s", name)
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

// ExpandVolume resizes an Azure Managed Disk to newSizeBytes.
func (p *AzureProvider) ExpandVolume(volumeID string, newSizeBytes int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	name := p.diskName(volumeID)

	const gib = 1024 * 1024 * 1024
	newSizeGiB := int32((newSizeBytes + gib - 1) / gib)
	if newSizeGiB == 0 {
		newSizeGiB = 1
	}

	resp, err := p.disksClient.Get(ctx, p.config.ResourceGroup, name, nil)
	if err != nil {
		return fmt.Errorf("failed to get disk %s for expansion: %w", name, err)
	}
	if resp.Properties != nil && resp.Properties.DiskSizeGB != nil && *resp.Properties.DiskSizeGB >= newSizeGiB {
		logger.Printf("Azure Managed Disk %s already at %d GiB >= requested %d GiB, skipping",
			name, *resp.Properties.DiskSizeGB, newSizeGiB)
		return nil
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

// CreateSnapshot creates an Azure snapshot from the given volume.
func (p *AzureProvider) CreateSnapshot(ctx context.Context, volumeID, snapshotID string) (*provider.SnapshotInfo, error) {

	snapshotsClient, err := armcompute.NewSnapshotsClient(p.config.SubscriptionID, p.credential(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create snapshots client: %w", err)
	}

	sourceDiskName := p.diskName(volumeID)
	sourceResourceID := p.diskResourceID(sourceDiskName)
	snapName := "csi-snap-" + snapshotID

	logger.Printf("Creating Azure snapshot %s from disk %s", snapName, sourceDiskName)

	poller, err := snapshotsClient.BeginCreateOrUpdate(ctx, p.config.ResourceGroup, snapName,
		armcompute.Snapshot{
			Location: to.Ptr(p.config.Location),
			Properties: &armcompute.SnapshotProperties{
				CreationData: &armcompute.CreationData{
					CreateOption:     to.Ptr(armcompute.DiskCreateOptionCopy),
					SourceResourceID: to.Ptr(sourceResourceID),
				},
			},
			Tags: map[string]*string{
				volumeTagKey:          to.Ptr(volumeID),
				"caa-csi-snapshot-id": to.Ptr(snapshotID),
			},
		}, nil)
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
func (p *AzureProvider) DeleteSnapshot(ctx context.Context, snapshotID string) error {

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

// ListSnapshots lists Azure snapshots. If volumeID is non-empty, only snapshots
// for that volume are returned; otherwise all managed snapshots are listed.
func (p *AzureProvider) ListSnapshots(ctx context.Context, volumeID string) ([]*provider.SnapshotInfo, error) {
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
			if _, ok := s.Tags["caa-csi-snapshot-id"]; !ok {
				continue
			}
			if volumeID != "" {
				tagVal, ok := s.Tags[volumeTagKey]
				if !ok || tagVal == nil || *tagVal != volumeID {
					continue
				}
			}
			snapID := ""
			if v, ok := s.Tags["caa-csi-snapshot-id"]; ok && v != nil {
				snapID = *v
			}
			sourceVolID := ""
			if v, ok := s.Tags[volumeTagKey]; ok && v != nil {
				sourceVolID = *v
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
				SourceVolumeID: sourceVolID,
				SizeBytes:      sizeBytes,
				CreationTime:   creationTime,
				ReadyToUse:     s.Properties != nil && s.Properties.ProvisioningState != nil && *s.Properties.ProvisioningState == "Succeeded",
			})
		}
	}
	return snaps, nil
}

// FindSnapshot looks up a single snapshot by its CSI snapshot name.
// Returns nil, nil if the snapshot does not exist.
func (p *AzureProvider) FindSnapshot(ctx context.Context, snapshotID string) (*provider.SnapshotInfo, error) {
	snapshotsClient, err := armcompute.NewSnapshotsClient(p.config.SubscriptionID, p.credential(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create snapshots client: %w", err)
	}

	snapName := "csi-snap-" + snapshotID
	result, err := snapshotsClient.Get(ctx, p.config.ResourceGroup, snapName, nil)
	if err != nil {
		if strings.Contains(err.Error(), "ResourceNotFound") || strings.Contains(err.Error(), "NotFound") {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get snapshot %s: %w", snapName, err)
	}

	sourceVolID := ""
	if result.Tags != nil {
		if v, ok := result.Tags[volumeTagKey]; ok && v != nil {
			sourceVolID = *v
		}
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
		SourceVolumeID: sourceVolID,
		SizeBytes:      sizeBytes,
		CreationTime:   creationTime,
		ReadyToUse:     result.Properties != nil && result.Properties.ProvisioningState != nil && *result.Properties.ProvisioningState == "Succeeded",
	}, nil
}

func (p *AzureProvider) credential() *azidentity.DefaultAzureCredential {
	return p.cred
}
