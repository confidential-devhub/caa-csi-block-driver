// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
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
	pollInterval   = 5 * time.Second
	pollTimeout    = 2 * time.Minute
	// Azure Managed Disk names: 1-80 chars, alphanumeric, underscores, hyphens, periods.
	maxDiskNameLen = 80
)

var disallowedChars = regexp.MustCompile(`[^a-zA-Z0-9_.\-]`)

type Config struct {
	SubscriptionID string
	ResourceGroup  string
	Location       string
	DiskSKU        string
}

type AzureProvider struct {
	disksClient *armcompute.DisksClient
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

	diskSKU := params["azureDiskSKU"]
	if diskSKU == "" {
		diskSKU = defaultDiskSKU
	}

	cfg := Config{
		SubscriptionID: subscriptionID,
		ResourceGroup:  resourceGroup,
		Location:       location,
		DiskSKU:        diskSKU,
	}

	client, err := newDisksClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create Azure Disks client: %w", err)
	}

	return &AzureProvider{
		disksClient: client,
		config:      cfg,
	}, nil
}

func newDisksClient(cfg Config) (*armcompute.DisksClient, error) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create default credential: %w", err)
	}

	return armcompute.NewDisksClient(cfg.SubscriptionID, cred, nil)
}

// sanitizeDiskName produces an Azure-safe disk name from an arbitrary CSI volume ID.
// Azure Managed Disk names must be 1-80 chars and contain only alphanumeric, underscore,
// hyphen, and period characters. If the sanitized name would exceed the limit, it is
// truncated and a content hash is appended to preserve uniqueness.
func sanitizeDiskName(volumeID string) string {
	name := "csi-vol-" + disallowedChars.ReplaceAllString(volumeID, "-")

	if len(name) <= maxDiskNameLen {
		return name
	}

	hash := sha256.Sum256([]byte(volumeID))
	suffix := "-" + hex.EncodeToString(hash[:8])
	return name[:maxDiskNameLen-len(suffix)] + suffix
}

func (p *AzureProvider) CreateVolume(volumeID string, sizeBytes int64) (*provider.VolumeInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
	defer cancel()

	exists, err := p.VolumeExists(volumeID)
	if err != nil {
		return nil, err
	}
	if exists {
		logger.Printf("Volume %s already exists, reusing", volumeID)
		return p.GetVolumeInfo(volumeID)
	}

	const gib int64 = 1024 * 1024 * 1024
	sizeGiB := int32((sizeBytes + gib - 1) / gib)
	if sizeGiB == 0 {
		sizeGiB = 1
	}

	name := sanitizeDiskName(volumeID)
	logger.Printf("Creating Azure Managed Disk %s (%d GiB, sku=%s, location=%s)",
		volumeID, sizeGiB, p.config.DiskSKU, p.config.Location)

	poller, err := p.disksClient.BeginCreateOrUpdate(ctx, p.config.ResourceGroup, name,
		armcompute.Disk{
			Location: to.Ptr(p.config.Location),
			SKU: &armcompute.DiskSKU{
				Name: to.Ptr(armcompute.DiskStorageAccountTypes(p.config.DiskSKU)),
			},
			Properties: &armcompute.DiskProperties{
				DiskSizeGB:          to.Ptr(sizeGiB),
				CreationData:        &armcompute.CreationData{CreateOption: to.Ptr(armcompute.DiskCreateOptionEmpty)},
				NetworkAccessPolicy: to.Ptr(armcompute.NetworkAccessPolicyAllowAll),
			},
			Tags: map[string]*string{
				volumeTagKey: to.Ptr(volumeID),
			},
		}, nil)
	if err != nil {
		return nil, fmt.Errorf("BeginCreateOrUpdate failed for %s: %w", volumeID, err)
	}

	disk, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("disk creation polling failed for %s: %w", volumeID, err)
	}

	if disk.ID == nil {
		return nil, fmt.Errorf("Azure API returned nil disk ID for %s", volumeID)
	}
	diskID := *disk.ID
	logger.Printf("Created Azure Managed Disk %s (disk-id=%s)", volumeID, diskID)

	return &provider.VolumeInfo{
		VolumeID:  volumeID,
		Path:      diskID,
		SizeBytes: sizeBytes,
		Provider:  "azure",
		Metadata: map[string]string{
			"cloud-volume-path": diskID,
			"cloud-provider":    "azure",
			"azure-disk-id":     diskID,
			"azure-disk-name":   name,
			"azure-location":    p.config.Location,
		},
	}, nil
}

func (p *AzureProvider) DeleteVolume(volumeID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
	defer cancel()

	name := sanitizeDiskName(volumeID)

	exists, err := p.VolumeExists(volumeID)
	if err != nil {
		return fmt.Errorf("failed to check if volume %s exists: %w", volumeID, err)
	}
	if !exists {
		logger.Printf("Volume %s not found, nothing to delete", volumeID)
		return nil
	}

	logger.Printf("Deleting Azure Managed Disk %s (name=%s)", volumeID, name)

	poller, err := p.disksClient.BeginDelete(ctx, p.config.ResourceGroup, name, nil)
	if err != nil {
		return fmt.Errorf("BeginDelete failed for %s: %w", name, err)
	}

	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("disk deletion polling failed for %s: %w", name, err)
	}

	logger.Printf("Deleted Azure Managed Disk %s", name)
	return nil
}

func (p *AzureProvider) GetVolumeInfo(volumeID string) (*provider.VolumeInfo, error) {
	ctx := context.TODO()
	name := sanitizeDiskName(volumeID)

	disk, err := p.disksClient.Get(ctx, p.config.ResourceGroup, name, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get disk %s: %w", name, err)
	}

	var sizeBytes int64
	if disk.Properties != nil && disk.Properties.DiskSizeGB != nil {
		sizeBytes = int64(*disk.Properties.DiskSizeGB) * 1024 * 1024 * 1024
	}

	if disk.ID == nil {
		return nil, fmt.Errorf("Azure API returned nil disk ID for %s", name)
	}
	diskID := *disk.ID
	return &provider.VolumeInfo{
		VolumeID:  volumeID,
		Path:      diskID,
		SizeBytes: sizeBytes,
		Provider:  "azure",
		Metadata: map[string]string{
			"cloud-volume-path": diskID,
			"cloud-provider":    "azure",
			"azure-disk-id":     diskID,
			"azure-disk-name":   name,
			"azure-location":    p.config.Location,
		},
	}, nil
}

func (p *AzureProvider) VolumeExists(volumeID string) (bool, error) {
	ctx := context.TODO()
	name := sanitizeDiskName(volumeID)

	_, err := p.disksClient.Get(ctx, p.config.ResourceGroup, name, nil)
	if err != nil {
		var respErr *azcore.ResponseError
		if errors.As(err, &respErr) && respErr.StatusCode == http.StatusNotFound {
			return false, nil
		}
		return false, fmt.Errorf("failed to check disk %s: %w", name, err)
	}
	return true, nil
}
