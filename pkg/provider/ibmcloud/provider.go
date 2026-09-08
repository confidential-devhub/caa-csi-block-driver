// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package ibmcloud

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"strconv"
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
	Profile       string   // e.g., "general-purpose", "5iops-tier", "10iops-tier", "custom"
	Iops          string   // Custom profile IOPS
	Encrypted     string   // "true" or "false"
	EncryptionKey string   // Key CRN
	BillingType   string   // e.g., "hourly"
	ExtraTags     []string // User-supplied extra tags
	FsType        string   // e.g., "ext4"
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
		return nil, fmt.Errorf("%w: %v", caaProvider.ErrInvalidParameters, err)
	}
	return newProviderWithConfig(cfg)
}

// Kept independent of Kubernetes/authentication so tests exercise the real parser.
func parseConfig(params map[string]string) (Config, error) {
	// Support both community keys (e.g. "region", "zone", "profile") and ibm-prefixed keys
	region := params["region"]
	if region == "" {
		region = params["ibmRegion"]
	}

	zone := params["zone"]
	if zone == "" {
		zone = params["ibmZone"]
	}

	profile := params["profile"]
	if profile == "" {
		profile = params["ibmProfile"]
	}
	if profile == "" {
		profile = "general-purpose" // default profile
	}

	resourceGroup := params["resourceGroup"]
	if resourceGroup == "" {
		resourceGroup = params["ibmResourceGroup"]
	}

	iops := params["iops"]
	if iops == "" {
		iops = params["ibmIops"]
	}

	var extraTags []string
	if tagsStr := params["tags"]; tagsStr != "" {
		for _, tag := range strings.Split(tagsStr, ",") {
			trimmed := strings.TrimSpace(tag)
			if trimmed != "" {
				extraTags = append(extraTags, trimmed)
			}
		}
	}

	cfg := Config{
		Region:        region,
		Zone:          zone,
		ResourceGroup: resourceGroup,
		Profile:       profile,
		Iops:          iops,
		Encrypted:     params["encrypted"],
		EncryptionKey: params["encryptionKey"],
		BillingType:   params["billingType"],
		ExtraTags:     extraTags,
		FsType:        params["csi.storage.k8s.io/fstype"],
	}
	if cfg.Region == "" || cfg.Zone == "" || cfg.ResourceGroup == "" {
		return Config{}, fmt.Errorf("region, zone and resourceGroup must be explicitly configured")
	}
	if !strings.HasPrefix(cfg.Zone, cfg.Region+"-") {
		return Config{}, fmt.Errorf("zone %q is not in region %q", cfg.Zone, cfg.Region)
	}
	switch cfg.Profile {
	case "general-purpose", "5iops-tier", "10iops-tier", "custom":
	default:
		return Config{}, fmt.Errorf("unsupported profile %q for this provisioning implementation", cfg.Profile)
	}
	if cfg.Profile == "custom" {
		iops, err := strconv.Atoi(cfg.Iops)
		if err != nil || iops <= 0 {
			return Config{}, fmt.Errorf("custom profile requires positive iops")
		}
		cfg.Iops = strconv.Itoa(iops)
	} else if cfg.Iops != "" {
		return Config{}, fmt.Errorf("iops is only supported with the custom profile")
	}
	if cfg.BillingType == "" {
		cfg.BillingType = "hourly"
	}
	if cfg.FsType == "" {
		cfg.FsType = "ext4"
	}
	if cfg.Encrypted == "" {
		cfg.Encrypted = "false"
	}
	cfg.Encrypted = strings.ToLower(cfg.Encrypted)
	if cfg.Encrypted != "true" && cfg.Encrypted != "false" {
		return Config{}, fmt.Errorf("encrypted must be true or false")
	}
	if (cfg.Encrypted == "true") != (cfg.EncryptionKey != "") {
		return Config{}, fmt.Errorf("encrypted=true requires encryptionKey; omit the key when encrypted=false")
	}
	for _, tag := range cfg.ExtraTags {
		if strings.HasPrefix(tag, ownershipTagPrefix) || strings.HasPrefix(tag, configTagPrefix) {
			return Config{}, fmt.Errorf("tag prefix %q is reserved for driver ownership", ownershipTagPrefix)
		}
	}
	return cfg, nil
}

func newProviderWithConfig(cfg Config) (*IBMCloudProvider, error) {
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
	if p.config.Zone == "" {
		return nil, fmt.Errorf("zone (or ibmZone) parameter is required to create a new volume")
	}

	if volumeID == "" {
		return nil, fmt.Errorf("%w: empty volume name", caaProvider.ErrInvalidParameters)
	}
	allocated, err := p.NormalizeCapacity(sizeBytes)
	if err != nil {
		return nil, err
	}
	vol, err := p.lookupVolume(volumeID)
	if err != nil && !errors.Is(err, caaProvider.ErrVolumeNotFound) {
		return nil, err
	}
	if vol != nil {
		if err := p.checkOwnedVolume(vol, volumeID); err != nil {
			return nil, err
		}
		info, err := volumeInfo(vol)
		if err != nil {
			return nil, err
		}
		if info.SizeBytes < allocated {
			return nil, fmt.Errorf("%w: %s is smaller than requested", caaProvider.ErrVolumeAlreadyExists, volumeID)
		}
		return info, nil
	}
	sizeGB := int(allocated / gib)

	logger.Printf("Creating IBM VPC Volume %s (%d GB, profile=%s, zone=%s)",
		volumeID, sizeGB, p.config.Profile, p.config.Zone)

	// Combine volume tagging
	tags := []string{ownershipTagPrefix + volumeID, p.configTag()}
	tags = append(tags, p.config.ExtraTags...)

	// Create volume request payload using community structures
	volRequest := provider.Volume{
		Name:        &volumeID,
		Capacity:    &sizeGB,
		Az:          p.config.Zone,
		Region:      p.config.Region,
		BillingType: p.config.BillingType,
		VPCVolume: provider.VPCVolume{
			Profile: &provider.Profile{
				Name: p.config.Profile,
			},
			Tags: tags,
		},
	}

	// Setup custom FS type if specified
	if p.config.FsType != "" {
		volRequest.VolumeType = provider.VolumeType(p.config.FsType)
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

	// Setup customer managed encryption key if specified
	if strings.ToLower(p.config.Encrypted) == "true" && p.config.EncryptionKey != "" {
		volRequest.VPCVolume.VolumeEncryptionKey = &provider.VolumeEncryptionKey{
			CRN: p.config.EncryptionKey,
		}
	}

	volResponse, err := p.session.CreateVolume(volRequest)
	if err != nil {
		return nil, fmt.Errorf("failed to CreateVolume via ibmcloud-volume-vpc SDK: %w", err)
	}
	// This SDK waits for availability but returns the original (possibly
	// pending) create object. Refresh by native ID before reporting success.
	if volResponse != nil && volResponse.VolumeID != "" && volResponse.Status != "available" {
		volResponse, err = p.session.GetVolume(volResponse.VolumeID)
		if err != nil {
			return nil, fmt.Errorf("refreshing created IBM volume: %w", err)
		}
	}
	// SDK v1.1.23's conversion omits Name even on a successful response.
	if volResponse != nil && volResponse.Name == nil {
		volResponse.Name = &volumeID
	}

	info, err := volumeInfo(volResponse)
	if err != nil {
		return nil, err
	}
	if info.VolumeID != volumeID || info.SizeBytes < allocated {
		return nil, fmt.Errorf("IBM create response does not match the requested name/capacity")
	}
	logger.Printf("Created IBM VPC Volume %s (vpc-id=%s)", volumeID, info.Path)
	return info, nil
}

// DeleteVolume removes an IBM Cloud VPC Block Storage volume.
func (p *IBMCloudProvider) DeleteVolume(volumeID string) error {
	vol, err := p.lookupVolume(volumeID)
	if errors.Is(err, caaProvider.ErrVolumeNotFound) {
		return nil
	}
	if err != nil {
		return err
	}

	// Cleanup needs verified ownership and identity, not a runnable volume.
	// In particular, a failed create must not prevent an API deletion attempt.
	if err := p.checkOwnedVolume(vol, volumeID); err != nil {
		return err
	}
	if vol.VolumeID == "" {
		return fmt.Errorf("IBM volume has no native ID")
	}
	logger.Printf("Deleting IBM VPC Volume %s (vpc-id=%s)", volumeID, vol.VolumeID)

	volRequest := &provider.Volume{
		VolumeID: vol.VolumeID,
	}

	err = p.session.DeleteVolume(volRequest)
	if err != nil {
		return fmt.Errorf("failed to DeleteVolume via ibmcloud-volume-vpc SDK: %w", err)
	}

	logger.Printf("Deleted IBM VPC Volume %s", vol.VolumeID)
	return nil
}

// GetVolumeInfo returns metadata about an existing volume by scanning for its name.
func (p *IBMCloudProvider) GetVolumeInfo(volumeID string) (*caaProvider.VolumeInfo, error) {
	vol, err := p.lookupVolume(volumeID)
	if err != nil {
		return nil, err
	}
	if err := p.checkOwnedVolume(vol, volumeID); err != nil {
		return nil, err
	}
	return volumeInfo(vol)
}

const (
	gib                int64 = 1024 * 1024 * 1024
	ownershipTagPrefix       = "caa-csi-volume-id:"
	configTagPrefix          = "caa-csi-config:"
)

// Persist the requested settings the pinned SDK omits from its returned model
// (notably resource group and encryption key). This is an idempotency marker,
// not a security attestation. Recovery additionally filters placement via the API.
func (p *IBMCloudProvider) configTag() string {
	cfg := p.config
	cfg.ExtraTags = nil
	data, _ := json.Marshal(cfg)
	return fmt.Sprintf("%s%x", configTagPrefix, sha256.Sum256(data))
}

func (p *IBMCloudProvider) NormalizeCapacity(sizeBytes int64) (int64, error) {
	if sizeBytes < 0 {
		return 0, fmt.Errorf("%w: negative capacity", caaProvider.ErrInvalidParameters)
	}
	units := sizeBytes / gib
	if sizeBytes%gib != 0 {
		units++
	}
	if units < 10 {
		units = 10
	}
	if units > math.MaxInt64/gib {
		return 0, fmt.Errorf("%w: capacity overflows int64", caaProvider.ErrInvalidParameters)
	}
	return units * gib, nil
}

// List-by-name returns an empty collection for absence, unlike GetVolumeByName
// whose SDK error wrapping obscures not-found versus authorization/transport errors.
func (p *IBMCloudProvider) lookupVolume(name string) (*provider.Volume, error) {
	if name == "" {
		return nil, fmt.Errorf("%w: empty volume name", caaProvider.ErrInvalidParameters)
	}
	// Names are unique across an account/region. Filtering by the requested
	// placement here would hide a same-name disk with conflicting settings.
	// Discover identity first, then check ownership/configuration separately.
	volumes, err := p.listVolumes(map[string]string{"name": name})
	if err != nil {
		return nil, err
	}
	var found *provider.Volume
	for _, vol := range volumes {
		if vol == nil {
			return nil, fmt.Errorf("IBM name lookup returned a nil volume")
		}
		// The server-side exact-name filter identifies the result when the SDK
		// drops Name. Preserve that identity in the generic model.
		if vol.Name == nil {
			vol.Name = &name
		}
		if *vol.Name != name {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("%w: ambiguous cloud name %s", caaProvider.ErrVolumeAlreadyExists, name)
		}
		found = vol
	}
	if found == nil {
		return nil, fmt.Errorf("%w: %s", caaProvider.ErrVolumeNotFound, name)
	}
	return found, nil
}

func (p *IBMCloudProvider) listVolumes(filters map[string]string) ([]*provider.Volume, error) {
	var volumes []*provider.Volume
	seen := map[string]bool{}
	for start := ""; ; {
		if seen[start] {
			return nil, fmt.Errorf("IBM list returned a repeated pagination token")
		}
		seen[start] = true
		page, err := p.session.ListVolumes(100, start, filters)
		if err != nil {
			return nil, fmt.Errorf("listing IBM volumes: %w", err)
		}
		if page == nil {
			return nil, fmt.Errorf("IBM list returned a nil response")
		}
		volumes = append(volumes, page.Volumes...)
		if page.Next == "" {
			return volumes, nil
		}
		start = page.Next
	}
}

func (p *IBMCloudProvider) checkOwnedVolume(vol *provider.Volume, name string) error {
	owned := false
	for _, tag := range vol.Tags {
		if tag == ownershipTagPrefix+name {
			owned = true
		}
	}
	if !owned {
		return fmt.Errorf("%w: %s lacks this driver's ownership tag", caaProvider.ErrVolumeAlreadyExists, name)
	}
	configMatches := false
	for _, tag := range vol.Tags {
		if tag == p.configTag() {
			configMatches = true
		}
	}
	if !configMatches {
		return fmt.Errorf("%w: %s lacks a matching configuration marker", caaProvider.ErrVolumeAlreadyExists, name)
	}
	if vol.Az != p.config.Zone || vol.Profile == nil || vol.Profile.Name != p.config.Profile ||
		(vol.ResourceGroup != nil && vol.ResourceGroup.ID != p.config.ResourceGroup) {
		return fmt.Errorf("%w: %s zone/profile/resource group mismatch", caaProvider.ErrVolumeAlreadyExists, name)
	}
	key := ""
	if vol.VolumeEncryptionKey != nil {
		key = vol.VolumeEncryptionKey.CRN
	}
	if vol.VolumeEncryptionKey != nil && key != p.config.EncryptionKey {
		return fmt.Errorf("%w: encryption key mismatch", caaProvider.ErrVolumeAlreadyExists)
	}
	if p.config.Profile == "custom" && (vol.Iops == nil || *vol.Iops != p.config.Iops) {
		return fmt.Errorf("%w: custom IOPS mismatch", caaProvider.ErrVolumeAlreadyExists)
	}
	return nil
}

func volumeInfo(vol *provider.Volume) (*caaProvider.VolumeInfo, error) {
	if vol == nil || vol.VolumeID == "" || vol.Name == nil || *vol.Name == "" ||
		vol.Capacity == nil || *vol.Capacity <= 0 || int64(*vol.Capacity) > math.MaxInt64/gib || vol.Profile == nil {
		return nil, fmt.Errorf("incomplete IBM volume response")
	}
	if vol.Status != "available" {
		return nil, fmt.Errorf("IBM volume %s is not available (status=%q); retry later", vol.VolumeID, vol.Status)
	}

	return &caaProvider.VolumeInfo{
		VolumeID:  *vol.Name,
		Path:      vol.VolumeID,
		SizeBytes: int64(*vol.Capacity) * gib,
		Provider:  "ibmcloud",
		Metadata: map[string]string{
			"cloud-volume-path": vol.VolumeID,
			"cloud-provider":    "ibmcloud",
			"ibm-volume-id":     vol.VolumeID,
			"zone":              vol.Az,
			"profile":           vol.Profile.Name,
		},
	}, nil
}

// VolumeExists checks if a volume with the given volume name tag exists.
func (p *IBMCloudProvider) VolumeExists(volumeID string) (bool, error) {
	_, err := p.GetVolumeInfo(volumeID)
	if errors.Is(err, caaProvider.ErrVolumeNotFound) {
		return false, nil
	}
	return err == nil, err
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

	cloudVolumes, err := p.listVolumes(map[string]string{
		"resource_group.id": p.config.ResourceGroup,
		"zone.name":         p.config.Zone,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to ListVolumes: %w", err)
	}

	for _, vol := range cloudVolumes {
		if vol == nil {
			continue
		}
		if vol.Name == nil {
			name := ""
			ambiguous := false
			for _, tag := range vol.Tags {
				if strings.HasPrefix(tag, ownershipTagPrefix) {
					if name != "" {
						ambiguous = true
					}
					name = strings.TrimPrefix(tag, ownershipTagPrefix)
				}
			}
			if name == "" || ambiguous {
				continue
			}
			vol.Name = &name
		}

		// A name containing '-' is NOT evidence of ownership. Never adopt an
		// unrelated IBM disk, even if it shares our PVC naming convention.
		if err := p.checkOwnedVolume(vol, *vol.Name); err != nil {
			continue
		}
		info, err := volumeInfo(vol)
		if err != nil {
			continue
		} // Pending/failed disks are not usable records.
		vols = append(vols, info)
	}

	return vols, nil
}
