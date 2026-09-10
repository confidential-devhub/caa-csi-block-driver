// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package ibmcloud

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/IBM/ibmcloud-volume-interface/lib/provider"
	providerError "github.com/IBM/ibmcloud-volume-interface/lib/utils"
	cloudProvider "github.com/IBM/ibmcloud-volume-vpc/pkg/ibmcloudprovider"
	k8sUtils "github.com/IBM/secret-utils-lib/pkg/k8s_utils"

	caaProvider "github.com/confidential-devhub/caa-csi-block-driver/pkg/provider"
)

const gib int64 = 1024 * 1024 * 1024

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
	Profile       string   // e.g., "general-purpose", "5iops-tier", "10iops-tier", "custom", "sdp"
	Iops          string   // Custom or SDP profile IOPS
	Encrypted     string   // "true" or "false"
	EncryptionKey string   // Key CRN
	BillingType   string   // e.g., "hourly"
	ExtraTags     []string // User-supplied extra tags
	FsType        string   // e.g., "ext4"
	Throughput    int32    // SDP bandwidth in Mbps
	VolumeID      string   // Pre-resolved volume ID from context
}

// IBMCloudProvider manages VPC block volumes using the official community ibmcloud-volume-vpc SDK.
type IBMCloudProvider struct {
	session provider.Session
	config  Config
}

// NewIBMCloudProvider parses StorageClass parameters and initializes the VPC SDK Storage Provider.
func NewIBMCloudProvider(params map[string]string) (*IBMCloudProvider, error) {
	cfg, err := parseConfig(params)
	if err != nil {
		return nil, err
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

	if providerConfig := storageProvider.GetConfig(); providerConfig != nil && providerConfig.VPC != nil {
		cfg.ResourceGroup = resolveResourceGroup(cfg.ResourceGroup, providerConfig.VPC.G2ResourceGroupID)
	}

	// Retrieve the active VPC Storage Provider Session
	session, err := storageProvider.GetProviderSession(context.TODO(), zapLogger)
	if err != nil {
		return nil, fmt.Errorf("failed to get Provider Session: %w", err)
	}

	return &IBMCloudProvider{session: session, config: cfg}, nil
}

func parseConfig(params map[string]string) (Config, error) {
	profile := firstParameter(params, "profile", "ibmProfile")
	if profile == "" {
		profile = "general-purpose"
	}

	var extraTags []string
	for _, tag := range strings.Split(params["tags"], ",") {
		if tag = strings.TrimSpace(tag); tag != "" {
			extraTags = append(extraTags, tag)
		}
	}

	volumeID := params["ibm-volume-id"]
	if volumeID == "" {
		volumeID = params["cloud-volume-path"]
	}

	cfg := Config{
		Region:        firstParameter(params, "region", "ibmRegion"),
		Zone:          firstParameter(params, "zone", "ibmZone"),
		ResourceGroup: firstParameter(params, "resourceGroup", "ibmResourceGroup"),
		Profile:       profile,
		Iops:          firstParameter(params, "iops", "ibmIops"),
		Encrypted:     params["encrypted"],
		EncryptionKey: params["encryptionKey"],
		BillingType:   params["billingType"],
		ExtraTags:     extraTags,
		FsType:        params["csi.storage.k8s.io/fstype"],
		VolumeID:      volumeID,
	}

	if throughput := params["throughput"]; throughput != "" {
		value, err := strconv.ParseInt(throughput, 10, 32)
		if err != nil || value <= 0 {
			return Config{}, fmt.Errorf("throughput must be a positive integer in Mbps")
		}
		cfg.Throughput = int32(value)
	}

	return cfg, nil
}

func firstParameter(params map[string]string, primary, fallback string) string {
	if value := params[primary]; value != "" {
		return value
	}
	return params[fallback]
}

func resolveResourceGroup(explicit, configured string) string {
	if explicit != "" {
		return explicit
	}
	return configured
}

// CreateVolume follows the IBM CSI driver's idempotency pattern: look up by
// name first, then create only when no matching volume exists.
func (p *IBMCloudProvider) CreateVolume(volumeID string, sizeBytes int64) (*caaProvider.VolumeInfo, error) {
	sizeGB, err := capacityGiB(sizeBytes)
	if err != nil {
		return nil, err
	}

	existing, err := p.getVolumeByName(volumeID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		logger.Printf("Volume %s already exists, reusing", volumeID)
		return volumeInfo(volumeID, existing), nil
	}

	logger.Printf("Creating IBM VPC Volume %s (%d GB, profile=%s, zone=%s)",
		volumeID, sizeGB, p.config.Profile, p.config.Zone)

	request := provider.Volume{
		Name:        &volumeID,
		Capacity:    &sizeGB,
		Az:          p.config.Zone,
		Region:      p.config.Region,
		BillingType: p.config.BillingType,
		VPCVolume: provider.VPCVolume{
			Bandwidth: p.config.Throughput,
			Profile:   &provider.Profile{Name: p.config.Profile},
			Tags:      append([]string{"caa-csi-volume-id:" + volumeID}, p.config.ExtraTags...),
		},
	}

	if p.config.FsType != "" {
		request.VolumeType = provider.VolumeType(p.config.FsType)
	}
	if p.config.ResourceGroup != "" {
		request.ResourceGroup = &provider.ResourceGroup{ID: p.config.ResourceGroup}
	}
	if p.config.Iops != "" && (p.config.Profile == "custom" || p.config.Profile == "sdp") {
		request.Iops = &p.config.Iops
	}
	if strings.EqualFold(p.config.Encrypted, "true") && p.config.EncryptionKey != "" {
		request.VolumeEncryptionKey = &provider.VolumeEncryptionKey{CRN: p.config.EncryptionKey}
	}

	created, err := p.session.CreateVolume(request)
	if err != nil {
		return nil, fmt.Errorf("failed to CreateVolume via ibmcloud-volume-vpc SDK: %w", err)
	}
	if created == nil || created.VolumeID == "" {
		return nil, fmt.Errorf("ibmcloud-volume-vpc SDK returned an empty volume")
	}

	logger.Printf("Created IBM VPC Volume %s (vpc-id=%s)", volumeID, created.VolumeID)
	return volumeInfo(volumeID, created), nil
}

// DeleteVolume resolves the CAA volume name to the native IBM volume ID before deletion.
func (p *IBMCloudProvider) DeleteVolume(volumeID string) error {
	volume, err := p.getVolumeByName(volumeID)
	if err != nil {
		return err
	}
	if volume == nil {
		logger.Printf("Volume %s not found, nothing to delete", volumeID)
		return nil
	}
	if volume.VolumeID == "" {
		return fmt.Errorf("IBM VPC volume %s has no native ID", volumeID)
	}

	logger.Printf("Deleting IBM VPC Volume %s (vpc-id=%s)", volumeID, volume.VolumeID)
	if err := p.session.DeleteVolume(&provider.Volume{VolumeID: volume.VolumeID}); err != nil {
		if isNotFound(err) {
			logger.Printf("Volume %s was already deleted", volume.VolumeID)
			return nil
		}
		return fmt.Errorf("failed to DeleteVolume via ibmcloud-volume-vpc SDK: %w", err)
	}

	logger.Printf("Deleted IBM VPC Volume %s", volume.VolumeID)
	return nil
}

// GetVolumeInfo returns metadata for an IBM volume found by its CAA volume name.
func (p *IBMCloudProvider) GetVolumeInfo(volumeID string) (*caaProvider.VolumeInfo, error) {
	volume, err := p.getVolumeByName(volumeID)
	if err != nil {
		return nil, err
	}
	if volume == nil {
		return nil, fmt.Errorf("volume %s not found", volumeID)
	}
	return volumeInfo(volumeID, volume), nil
}

// VolumeExists reports whether an IBM volume exists without hiding lookup failures.
func (p *IBMCloudProvider) VolumeExists(volumeID string) (bool, error) {
	volume, err := p.getVolumeByName(volumeID)
	if err != nil {
		return false, err
	}
	return volume != nil, nil
}

func (p *IBMCloudProvider) getVolumeByName(name string) (*provider.Volume, error) {
	if p.config.VolumeID != "" {
		volume, err := p.session.GetVolume(p.config.VolumeID)
		if err == nil {
			return volume, nil
		}
		if isNotFound(err) {
			return nil, nil
		}
		logger.Printf("Warning: failed to GetVolume %s, falling back to name lookup: %v", p.config.VolumeID, err)
	}

	volume, err := p.session.GetVolumeByName(name)
	if err == nil {
		return volume, nil
	}
	if isNotFound(err) {
		return nil, nil
	}
	return nil, fmt.Errorf("failed to GetVolumeByName: %w", err)
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if providerError.GetErrorType(err) == providerError.EntityNotFound {
		return true
	}
	var message providerError.Message
	return errors.As(err, &message) && message.RC == 404
}

func capacityGiB(sizeBytes int64) (int, error) {
	if sizeBytes <= 0 {
		return 0, fmt.Errorf("volume capacity must be greater than zero")
	}
	sizeGB := sizeBytes / gib
	if sizeBytes%gib != 0 {
		sizeGB++
	}
	return int(sizeGB), nil
}

func volumeInfo(volumeID string, volume *provider.Volume) *caaProvider.VolumeInfo {
	var capacityBytes int64
	if volume.Capacity != nil {
		capacityBytes = int64(*volume.Capacity) * gib
	}

	profile := ""
	if volume.Profile != nil {
		profile = volume.Profile.Name
	}

	return &caaProvider.VolumeInfo{
		VolumeID:  volumeID,
		Path:      volume.VolumeID,
		SizeBytes: capacityBytes,
		Provider:  "ibmcloud",
		Metadata: map[string]string{
			"cloud-volume-path": volume.VolumeID,
			"cloud-provider":    "ibmcloud",
			"ibm-volume-id":     volume.VolumeID,
			"zone":              volume.Az,
			"profile":           profile,
		},
	}
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
