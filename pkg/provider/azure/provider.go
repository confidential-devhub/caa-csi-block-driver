// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"fmt"
	"log"
	"regexp"
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

	return &AzureProvider{
		disksClient: disksClient,
		cred:        cred,
		config: Config{
			SubscriptionID: subscriptionID,
			ResourceGroup:  resourceGroup,
			Location:       location,
			DiskSKU:        diskSKU,
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

	disk := armcompute.Disk{
		Location: to.Ptr(p.config.Location),
		SKU: &armcompute.DiskSKU{
			Name: to.Ptr(armcompute.DiskStorageAccountTypes(p.config.DiskSKU)),
		},
		Properties: &armcompute.DiskProperties{
			CreationData: &armcompute.CreationData{
				CreateOption: to.Ptr(armcompute.DiskCreateOptionEmpty),
			},
			DiskSizeGB: to.Ptr(sizeGiB),
		},
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

func (p *AzureProvider) credential() *azidentity.DefaultAzureCredential {
	return p.cred
}
