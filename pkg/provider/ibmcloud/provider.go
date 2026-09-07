// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package ibmcloud

import (
	"context"
	"fmt"
	"log"
	"strings"

	"go.uber.org/zap"

	"github.com/IBM/ibmcloud-volume-interface/lib/provider"
	cloudProvider "github.com/IBM/ibmcloud-volume-vpc/pkg/ibmcloudprovider"
	k8sUtils "github.com/IBM/secret-utils-lib/pkg/k8s_utils"

	caaProvider "github.com/confidential-devhub/caa-csi-block-driver/pkg/provider"
)

var logger = log.New(log.Writer(), "[caa-csi/ibmcloud] ", log.LstdFlags|log.Lmsgprefix)

// Ensure interfaces are fully implemented
var _ caaProvider.BlockVolumeProvider = (*IBMCloudProvider)(nil)
var _ caaProvider.VolumeExpander = (*IBMCloudProvider)(nil)
var _ caaProvider.VolumeRecoverer = (*IBMCloudProvider)(nil)

func init() {
	// Register the ibmcloud provider with the CSI Driver factory
	caaProvider.RegisterProvider("ibmcloud", func(params map[string]string) (caaProvider.BlockVolumeProvider, error) {
		return NewIBMCloudProvider(params)
	})
}

// Config holds IBM Cloud VPC Storage configurations parsed from the StorageClass parameters.
type Config struct {
	Region        string
	Zone          string
	ResourceGroup string
	Profile       string // e.g., "general-purpose", "5iops-tier", "10iops-tier", "custom"
	Iops          string // Custom profile IOPS
}

// IBMCloudProvider manages VPC block volumes using the official community ibmcloud-volume-vpc SDK.
type IBMCloudProvider struct {
	session provider.Session
	config  Config
}

// NewIBMCloudProvider parses StorageClass parameters and initializes the VPC SDK Storage Provider.
func NewIBMCloudProvider(params map[string]string) (*IBMCloudProvider, error) {
	region := params["ibmRegion"]
	if region == "" {
		region = params["region"] // fallback
	}
	if region == "" {
		return nil, fmt.Errorf("ibmRegion is required for ibmcloud provider")
	}

	zone := params["ibmZone"]
	if zone == "" {
		zone = params["zone"] // fallback
	}
	if zone == "" {
		return nil, fmt.Errorf("ibmZone is required for ibmcloud provider")
	}

	profile := params["ibmProfile"]
	if profile == "" {
		profile = params["profile"] // fallback
	}
	if profile == "" {
		profile = "general-purpose" // default profile
	}

	cfg := Config{
		Region:        region,
		Zone:          zone,
		ResourceGroup: params["ibmResourceGroup"],
		Profile:       profile,
		Iops:          params["ibmIops"],
	}

	// Instantiate a production Zap logger for the SDK
	zapLogger, err := zap.NewProduction()
	if err != nil {
		// Fallback to development logger if production fails
		zapLogger = zap.NewExample()
	}

	// Instantiate Kubernetes client from service account
	k8sClient, err := k8sUtils.Getk8sClientSet()
	if err != nil {
		return nil, fmt.Errorf("failed to get Kubernetes clientset: %w", err)
	}

	// Initialize the official community IBM Cloud Storage Provider
	storageProvider, err := cloudProvider.NewIBMCloudStorageProvider("", &k8sClient, zapLogger)
	if err != nil {
		return nil, fmt.Errorf("failed to create NewIBMCloudStorageProvider: %w", err)
	}

	// Retrieve the active VPC Storage Provider Session
	session, err := storageProvider.GetProviderSession(context.TODO(), zapLogger)
	if err != nil {
		return nil, fmt.Errorf("failed to get Provider Session: %w", err)
	}

	return &IBMCloudProvider{
		session: session,
		config:  cfg,
	}, nil
}

// CreateVolume provisions a new VPC Block Volume using the ibmcloud-volume-vpc SDK.
func (p *IBMCloudProvider) CreateVolume(volumeID string, sizeBytes int64) (*caaProvider.VolumeInfo, error) {
	exists, err := p.VolumeExists(volumeID)
	if err != nil {
		return nil, err
	}
	if exists {
		logger.Printf("Volume %s already exists, reusing", volumeID)
		return p.GetVolumeInfo(volumeID)
	}

	sizeGB := int(sizeBytes / (1024 * 1024 * 1024))
	if sizeGB == 0 {
		sizeGB = 10 // Minimum default fallback size
	}

	logger.Printf("Creating IBM VPC Volume %s (%d GB, profile=%s, zone=%s)", 
		volumeID, sizeGB, p.config.Profile, p.config.Zone)

	// Create volume request payload using community structures
	volRequest := provider.Volume{
		Name:     &volumeID,
		Capacity: &sizeGB,
		Az:       p.config.Zone,
		Region:   p.config.Region,
		VPCVolume: provider.VPCVolume{
			Profile: &provider.Profile{
				Name: p.config.Profile,
			},
			Tags: []string{
				"caa-csi-volume-id:" + volumeID,
			},
		},
	}

	// Setup Custom Resource Group if specified
	if p.config.ResourceGroup != "" {
		volRequest.VPCVolume.ResourceGroup = &provider.ResourceGroup{
			ID: p.config.ResourceGroup,
		}
	}

	// Setup custom IOPS if specified and profile is custom
	if p.config.Iops != "" && p.config.Profile == "custom" {
		volRequest.Iops = &p.config.Iops
	}

	volResponse, err := p.session.CreateVolume(volRequest)
	if err != nil {
		return nil, fmt.Errorf("failed to CreateVolume via ibmcloud-volume-vpc SDK: %w", err)
	}

	logger.Printf("Created IBM VPC Volume %s (vpc-id=%s)", volumeID, volResponse.VolumeID)

	return &caaProvider.VolumeInfo{
		VolumeID:  volumeID,
		Path:      volResponse.VolumeID,
		SizeBytes: sizeBytes,
		Provider:  "ibmcloud",
		Metadata: map[string]string{
			"cloud-volume-path": volResponse.VolumeID,
			"cloud-provider":    "ibmcloud",
			"ibm-volume-id":     volResponse.VolumeID,
			"zone":              p.config.Zone,
			"profile":           p.config.Profile,
		},
	}, nil
}

// DeleteVolume removes an IBM Cloud VPC Block Storage volume.
func (p *IBMCloudProvider) DeleteVolume(volumeID string) error {
	volInfo, err := p.GetVolumeInfo(volumeID)
	if err != nil {
		logger.Printf("Volume %s not found, nothing to delete (idempotent)", volumeID)
		return nil
	}

	logger.Printf("Deleting IBM VPC Volume %s (vpc-id=%s)", volumeID, volInfo.Path)

	volRequest := &provider.Volume{
		VolumeID: volInfo.Path,
	}

	err = p.session.DeleteVolume(volRequest)
	if err != nil {
		return fmt.Errorf("failed to DeleteVolume via ibmcloud-volume-vpc SDK: %w", err)
	}

	logger.Printf("Deleted IBM VPC Volume %s", volInfo.Path)
	return nil
}

// GetVolumeInfo returns metadata about an existing volume by scanning for its name.
func (p *IBMCloudProvider) GetVolumeInfo(volumeID string) (*caaProvider.VolumeInfo, error) {
	vol, err := p.session.GetVolumeByName(volumeID)
	if err != nil {
		return nil, fmt.Errorf("failed to GetVolumeByName: %w", err)
	}

	if vol == nil {
		return nil, fmt.Errorf("volume %s not found", volumeID)
	}

	var capacityBytes int64
	if vol.Capacity != nil {
		capacityBytes = int64(*vol.Capacity) * 1024 * 1024 * 1024
	}

	profileName := ""
	if vol.VPCVolume.Profile != nil {
		profileName = vol.VPCVolume.Profile.Name
	}

	return &caaProvider.VolumeInfo{
		VolumeID:  volumeID,
		Path:      vol.VolumeID,
		SizeBytes: capacityBytes,
		Provider:  "ibmcloud",
		Metadata: map[string]string{
			"cloud-volume-path": vol.VolumeID,
			"cloud-provider":    "ibmcloud",
			"ibm-volume-id":     vol.VolumeID,
			"zone":              vol.Az,
			"profile":           profileName,
		},
	}, nil
}

// VolumeExists checks if a volume with the given volume name tag exists.
func (p *IBMCloudProvider) VolumeExists(volumeID string) (bool, error) {
	vol, err := p.session.GetVolumeByName(volumeID)
	if err != nil {
		return false, nil
	}
	return vol != nil, nil
}

// ExpandVolume implements VolumeExpander to support online expansion.
func (p *IBMCloudProvider) ExpandVolume(volumeID string, newSizeBytes int64) error {
	volInfo, err := p.GetVolumeInfo(volumeID)
	if err != nil {
		return fmt.Errorf("cannot expand volume %s: %w", volumeID, err)
	}

	newSizeGB := int((newSizeBytes + (1024*1024*1024 - 1)) / (1024 * 1024 * 1024))
	logger.Printf("Expanding IBM VPC volume %s (vpc-id=%s) to %d GB", volumeID, volInfo.Path, newSizeGB)

	volRequest := provider.Volume{
		VolumeID: volInfo.Path,
		Capacity: &newSizeGB,
	}

	err = p.session.UpdateVolume(volRequest)
	if err != nil {
		return fmt.Errorf("failed to expand volume via ibmcloud-volume-vpc SDK: %w", err)
	}

	logger.Printf("IBM VPC volume %s expanded successfully", volInfo.Path)
	return nil
}

// ListManagedVolumes implements VolumeRecoverer so caa-csi recovers state after restarts.
func (p *IBMCloudProvider) ListManagedVolumes() ([]*caaProvider.VolumeInfo, error) {
	var vols []*caaProvider.VolumeInfo

	vList, err := p.session.ListVolumes(100, "", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to ListVolumes: %w", err)
	}

	if vList == nil {
		return vols, nil
	}

	for _, vol := range vList.Volumes {
		if vol == nil || vol.Name == nil {
			continue
		}
		
		name := *vol.Name
		// Only recover volumes created with caa-csi block prefix
		if !strings.HasPrefix(name, "csi-vol-") && !strings.Contains(name, "-") {
			continue
		}

		csiVolumeID := name
		if strings.HasPrefix(name, "csi-vol-") {
			csiVolumeID = strings.TrimPrefix(name, "csi-vol-")
		}

		var capacityBytes int64
		if vol.Capacity != nil {
			capacityBytes = int64(*vol.Capacity) * 1024 * 1024 * 1024
		}

		profileName := ""
		if vol.VPCVolume.Profile != nil {
			profileName = vol.VPCVolume.Profile.Name
		}

		vols = append(vols, &caaProvider.VolumeInfo{
			VolumeID:  csiVolumeID,
			Path:      vol.VolumeID,
			SizeBytes: capacityBytes,
			Provider:  "ibmcloud",
			Metadata: map[string]string{
				"cloud-volume-path": vol.VolumeID,
				"cloud-provider":    "ibmcloud",
				"ibm-volume-id":     vol.VolumeID,
				"zone":              vol.Az,
				"profile":           profileName,
			},
		})
	}

	return vols, nil
}
