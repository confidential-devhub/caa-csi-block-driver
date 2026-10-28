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
	"time"

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
var _ caaProvider.VolumeSnapshotter = (*IBMCloudProvider)(nil)
var _ caaProvider.VolumeCloner = (*IBMCloudProvider)(nil)

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
	Throughput    int32    `json:",omitempty"` // Mbps; omit zero to preserve existing configuration tags
	VolumeID      string   `json:",omitempty"` // Pre-resolved volume ID from context
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
	p, err := newProviderWithConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", caaProvider.ErrInvalidParameters, err)
	}
	return p, nil
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

	volumeID := params["ibm-volume-id"]
	if volumeID == "" {
		volumeID = params["cloud-volume-path"]
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
		VolumeID:      volumeID,
	}
	if cfg.Region == "" || cfg.Zone == "" {
		return Config{}, fmt.Errorf("region and zone must be explicitly configured")
	}
	if cfg.Region == "" || cfg.Zone == "" {
		return Config{}, fmt.Errorf("region and zone must be explicitly configured")
	}
	// The VPC API validates profile availability and zone placement.
	// Like the upstream driver, ignore IOPS for tiered profiles.
	if cfg.Profile != "custom" && cfg.Profile != "sdp" {
		cfg.Iops = ""
	}
	if cfg.Iops != "" {
		iops, err := strconv.ParseInt(cfg.Iops, 10, 64)
		if err != nil || iops <= 0 {
			return Config{}, fmt.Errorf("iops must be a positive integer")
		}
		cfg.Iops = strconv.FormatInt(iops, 10)
	}
	// Match the upstream StorageClass key and SDK bandwidth units (Mbps).
	if value := params["throughput"]; value != "" {
		bandwidth, err := strconv.ParseInt(value, 10, 32)
		if err != nil || bandwidth <= 0 {
			return Config{}, fmt.Errorf("throughput must be a positive int32 (Mbps)")
		}
		cfg.Throughput = int32(bandwidth)
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

	validProfiles := map[string]bool{
		"general-purpose": true,
		"5iops-tier":      true,
		"10iops-tier":     true,
		"custom":          true,
		"sdp":             true,
	}
	if !validProfiles[cfg.Profile] {
		return Config{}, fmt.Errorf("unsupported profile: %q", cfg.Profile)
	}

	if !strings.HasPrefix(cfg.Zone, cfg.Region) {
		return Config{}, fmt.Errorf("zone %q must belong to region %q", cfg.Zone, cfg.Region)
	}

	if cfg.Profile == "custom" && cfg.Iops == "" {
		return Config{}, fmt.Errorf("custom profile requires iops")
	}

	if cfg.Profile != "custom" && cfg.Profile != "sdp" && cfg.Iops != "" {
		return Config{}, fmt.Errorf("iops is not supported for tiered profiles")
	}

	if cfg.Iops != "" {
		iops, err := strconv.ParseInt(cfg.Iops, 10, 64)
		if err != nil || iops <= 0 {
			return Config{}, fmt.Errorf("iops must be a positive integer")
		}
		cfg.Iops = strconv.FormatInt(iops, 10)
	}
	// Match the upstream StorageClass key and SDK bandwidth units (Mbps).
	if value := params["throughput"]; value != "" {
		bandwidth, err := strconv.ParseInt(value, 10, 32)
		if err != nil || bandwidth <= 0 {
			return Config{}, fmt.Errorf("throughput must be a positive int32 (Mbps)")
		}
		cfg.Throughput = int32(bandwidth)
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
	// The SDK requires a resource group in its request. Upstream defaults it
	// from this same provider configuration when the StorageClass omits it.
	var errRG error
	cfg.ResourceGroup, errRG = resolveResourceGroup(cfg.ResourceGroup, storageProvider.GetConfig().VPC.G2ResourceGroupID)
	if errRG != nil {
		return nil, errRG
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

// CreateVolume provisions a new VPC Block Volume using the ibmcloud-volume-vpc SDK.
func (p *IBMCloudProvider) CreateVolume(ctx context.Context, volumeID string, sizeBytes int64) (*caaProvider.VolumeInfo, error) {
	return p.createVolume(ctx, volumeID, sizeBytes, "")
}

// createVolume implements CreateVolume. When snapshotRef (a snapshot name or
// native ID) is non-empty, the new volume is restored from that snapshot.
func (p *IBMCloudProvider) createVolume(ctx context.Context, volumeID string, sizeBytes int64, snapshotRef string) (*caaProvider.VolumeInfo, error) {
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
	if info, err := p.existingVolumeInfo(ctx, volumeID, allocated); err != nil || info != nil {
		return info, err
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

// existingVolumeInfo returns the already-provisioned volume for volumeID, or
// nil, nil if it does not exist. It makes volume creation idempotent.
func (p *IBMCloudProvider) existingVolumeInfo(ctx context.Context, volumeID string, allocated int64) (*caaProvider.VolumeInfo, error) {
	vol, err := p.lookupVolume(ctx, volumeID)
	if errors.Is(err, caaProvider.ErrVolumeNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
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

// DeleteVolume removes an IBM Cloud VPC Block Storage volume.
func (p *IBMCloudProvider) DeleteVolume(ctx context.Context, volumeID string) error {
	vol, err := p.lookupVolume(ctx, volumeID)
	if errors.Is(err, caaProvider.ErrVolumeNotFound) {
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

// GetVolumeInfo returns metadata about an existing volume by scanning for its name.
func (p *IBMCloudProvider) GetVolumeInfo(ctx context.Context, volumeID string) (*caaProvider.VolumeInfo, error) {
	vol, err := p.lookupVolume(ctx, volumeID)
	if err != nil {
		return nil, err
	}
	if volume == nil {
		return nil, fmt.Errorf("volume %s not found", volumeID)
	}
	return volumeInfo(volumeID, volume), nil
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
	cfg.VolumeID = ""
	data, _ := json.Marshal(cfg)
	return fmt.Sprintf("%s%x", configTagPrefix, sha256.Sum256(data))
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

// List-by-name returns an empty collection for absence, unlike GetVolumeByName
// whose SDK error wrapping obscures not-found versus authorization/transport errors.
func (p *IBMCloudProvider) lookupVolume(ctx context.Context, name string) (*provider.Volume, error) {
	if p.config.VolumeID != "" {
		volume, err := p.session.GetVolume(p.config.VolumeID)
		if err == nil {
			if volume != nil && volume.Name == nil {
				volume.Name = &name
			}
			return volume, nil
		}
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %s", caaProvider.ErrVolumeNotFound, name)
		}
		logger.Printf("Warning: failed to GetVolume %s, falling back to name lookup: %v", p.config.VolumeID, err)
	}

	if name == "" {
		return nil, fmt.Errorf("%w: empty volume name", caaProvider.ErrInvalidParameters)
	}
	// Names are unique across an account/region. Filtering by the requested
	// placement here would hide a same-name disk with conflicting settings.
	// Discover identity first, then check ownership/configuration separately.
	volumes, err := p.listVolumes(ctx, map[string]string{"name": name})
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

func (p *IBMCloudProvider) listVolumes(ctx context.Context, filters map[string]string) ([]*provider.Volume, error) {
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
	if p.config.Iops != "" && (vol.Iops == nil || *vol.Iops != p.config.Iops) {
		return fmt.Errorf("%w: IOPS mismatch", caaProvider.ErrVolumeAlreadyExists)
	}
	if p.config.Throughput != 0 && vol.Bandwidth != p.config.Throughput {
		return fmt.Errorf("%w: throughput mismatch", caaProvider.ErrVolumeAlreadyExists)
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
	}, nil
}

// VolumeExists checks if a volume with the given volume name tag exists.
func (p *IBMCloudProvider) VolumeExists(ctx context.Context, volumeID string) (bool, error) {
	_, err := p.GetVolumeInfo(ctx, volumeID)
	if errors.Is(err, caaProvider.ErrVolumeNotFound) {
		return false, nil
	}
}

// ExpandVolume implements VolumeExpander to support online expansion.
func (p *IBMCloudProvider) ExpandVolume(ctx context.Context, volumeID string, newSizeBytes int64) error {
	volInfo, err := p.GetVolumeInfo(ctx, volumeID)
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
func (p *IBMCloudProvider) ListManagedVolumes(ctx context.Context) ([]*caaProvider.VolumeInfo, error) {
	var vols []*caaProvider.VolumeInfo

	cloudVolumes, err := p.listVolumes(ctx, map[string]string{
		"resource_group.id": p.config.ResourceGroup,
		"zone.name":         p.config.Zone,
	})
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

// Snapshot readiness polling used when an operation needs a stable snapshot
// (e.g. cloning a volume through a temporary snapshot).
var snapshotPollInterval = 5 * time.Second

const snapshotReadyTimeout = 5 * time.Minute

// The pinned SDK's snapshot model drops names and tags, so snapshots are
// identified as follows:
//   - CSI snapshot IDs returned to Kubernetes are the native IBM snapshot IDs.
//   - A CSI snapshot name is resolved with the server-side exact-name filter.
//   - The source CSI volume name is recovered from the source volume's
//     ownership tag; snapshots whose source volume is gone cannot be attributed.

// CreateSnapshot creates a VPC snapshot named snapshotID from the CSI volume volumeID.
// It does not wait for the snapshot to become stable; ReadyToUse reports its state.
func (p *IBMCloudProvider) CreateSnapshot(ctx context.Context, volumeID, snapshotID string) (*caaProvider.SnapshotInfo, error) {
	if snapshotID == "" {
		return nil, fmt.Errorf("%w: empty snapshot name", caaProvider.ErrInvalidParameters)
	}
	vol, err := p.lookupVolume(ctx, volumeID)
	if err != nil {
		return nil, fmt.Errorf("cannot find IBM volume for snapshot: %w", err)
	}
	if err := p.checkOwnedVolume(vol, volumeID); err != nil {
		return nil, err
	}
	if vol.VolumeID == "" {
		return nil, fmt.Errorf("IBM volume has no native ID")
	}

	existing, err := p.findSnapshotByName(ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if existing.VolumeID != vol.VolumeID {
			return nil, fmt.Errorf("%w: snapshot %s exists for a different source volume", caaProvider.ErrVolumeAlreadyExists, snapshotID)
		}
		return snapshotInfo(existing, volumeID), nil
	}

	logger.Printf("Creating IBM VPC snapshot %s from volume %s (vpc-id=%s)", snapshotID, volumeID, vol.VolumeID)
	snap, err := p.session.CreateSnapshot(vol.VolumeID, provider.SnapshotParameters{
		Name:          snapshotID,
		ResourceGroup: p.config.ResourceGroup,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to CreateSnapshot via ibmcloud-volume-vpc SDK: %w", err)
	}
	if snap == nil || snap.SnapshotID == "" {
		return nil, fmt.Errorf("incomplete IBM snapshot response")
	}
	return snapshotInfo(snap, volumeID), nil
}

// DeleteSnapshot deletes a snapshot by CSI snapshot name or native ID.
// A snapshot that does not exist is not an error.
func (p *IBMCloudProvider) DeleteSnapshot(ctx context.Context, snapshotID string) error {
	snap, err := p.resolveSnapshot(ctx, snapshotID)
	if err != nil {
		return fmt.Errorf("failed to look up snapshot %s for deletion: %w", snapshotID, err)
	}
	if snap == nil {
		logger.Printf("Snapshot %s not found, nothing to delete (idempotent)", snapshotID)
		return nil
	}

	logger.Printf("Deleting IBM VPC snapshot %s (vpc-id=%s)", snapshotID, snap.SnapshotID)
	if err := p.session.DeleteSnapshot(&provider.Snapshot{SnapshotID: snap.SnapshotID}); err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to DeleteSnapshot via ibmcloud-volume-vpc SDK: %w", err)
	}
	return nil
}

// ListSnapshots lists snapshots of the CSI volume volumeID, or, when volumeID is
// empty, all snapshots in the configured resource group whose source volume is
// owned by this driver.
func (p *IBMCloudProvider) ListSnapshots(ctx context.Context, volumeID string) ([]*caaProvider.SnapshotInfo, error) {
	filters := map[string]string{"resource_group.id": p.config.ResourceGroup}
	if volumeID != "" {
		vol, err := p.lookupVolume(ctx, volumeID)
		if errors.Is(err, caaProvider.ErrVolumeNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if err := p.checkOwnedVolume(vol, volumeID); err != nil {
			return nil, err
		}
		filters["source_volume.id"] = vol.VolumeID
	}

	snaps, err := p.listSnapshots(ctx, filters)
	if err != nil {
		return nil, err
	}
	sourceNames := map[string]string{}
	var infos []*caaProvider.SnapshotInfo
	for _, snap := range snaps {
		sourceName := volumeID
		if sourceName == "" {
			if sourceName, err = p.sourceVolumeName(snap.VolumeID, sourceNames); err != nil {
				return nil, err
			}
			if sourceName == "" {
				continue // Not provably ours.
			}
		}
		infos = append(infos, snapshotInfo(snap, sourceName))
	}
	return infos, nil
}

// FindSnapshot looks up a snapshot by CSI snapshot name or native ID.
// Returns nil, nil if the snapshot does not exist.
func (p *IBMCloudProvider) FindSnapshot(ctx context.Context, snapshotID string) (*caaProvider.SnapshotInfo, error) {
	snap, err := p.resolveSnapshot(ctx, snapshotID)
	if err != nil || snap == nil {
		return nil, err
	}
	sourceName, err := p.sourceVolumeName(snap.VolumeID, map[string]string{})
	if err != nil {
		return nil, err
	}
	return snapshotInfo(snap, sourceName), nil
}

// CreateVolumeFromSnapshot restores a new volume from an existing snapshot.
func (p *IBMCloudProvider) CreateVolumeFromSnapshot(ctx context.Context, volumeID, snapshotID string, sizeBytes int64) (*caaProvider.VolumeInfo, error) {
	return p.createVolume(ctx, volumeID, sizeBytes, snapshotID)
}

// CreateVolumeFromVolume clones a volume by taking a temporary snapshot of the
// source, restoring a new volume from it, and deleting the temporary snapshot.
func (p *IBMCloudProvider) CreateVolumeFromVolume(ctx context.Context, volumeID, sourceVolumeID string, sizeBytes int64) (*caaProvider.VolumeInfo, error) {
	if volumeID == "" {
		return nil, fmt.Errorf("%w: empty volume name", caaProvider.ErrInvalidParameters)
	}
	allocated, err := p.NormalizeCapacity(sizeBytes)
	if err != nil {
		return nil, err
	}
	if info, err := p.existingVolumeInfo(ctx, volumeID, allocated); err != nil || info != nil {
		return info, err
	}

	tempName := "clone-" + volumeID
	logger.Printf("Cloning volume %s -> %s via temporary snapshot %s", sourceVolumeID, volumeID, tempName)
	snapInfo, err := p.CreateSnapshot(ctx, sourceVolumeID, tempName)
	if err != nil {
		return nil, fmt.Errorf("creating temp snapshot for clone: %w", err)
	}
	cleanup := func() {
		if err := p.DeleteSnapshot(ctx, snapInfo.SnapshotID); err != nil {
			logger.Printf("WARNING: failed to clean up temp clone snapshot %s: %v", tempName, err)
		}
	}

	if err := p.waitSnapshotReady(ctx, snapInfo.SnapshotID); err != nil {
		cleanup()
		return nil, fmt.Errorf("temp snapshot %s not ready: %w", tempName, err)
	}
	volInfo, err := p.createVolume(ctx, volumeID, sizeBytes, snapInfo.SnapshotID)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("creating volume from temp snapshot: %w", err)
	}
	cleanup()
	return volInfo, nil
}

func (p *IBMCloudProvider) waitSnapshotReady(ctx context.Context, nativeID string) error {
	ctx, cancel := context.WithTimeout(ctx, snapshotReadyTimeout)
	defer cancel()
	for {
		snap, err := p.session.GetSnapshot(nativeID)
		if err != nil {
			return err
		}
		if snap != nil && snap.ReadyToUse {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(snapshotPollInterval):
		}
	}
}

// restorableSnapshotID resolves ref and verifies it can seed a volume of the given size.
func (p *IBMCloudProvider) restorableSnapshotID(ctx context.Context, ref string, allocated int64) (string, error) {
	snap, err := p.resolveSnapshot(ctx, ref)
	if err != nil {
		return "", err
	}
	if snap == nil {
		return "", fmt.Errorf("snapshot %s not found", ref)
	}
	if !snap.ReadyToUse {
		return "", fmt.Errorf("IBM snapshot %s is not ready; retry later", snap.SnapshotID)
	}
	if snap.SnapshotSize > allocated {
		return "", fmt.Errorf("%w: volume size is smaller than snapshot %s", caaProvider.ErrInvalidParameters, snap.SnapshotID)
	}
	return snap.SnapshotID, nil
}

// resolveSnapshot finds a snapshot by CSI name first, then by native ID.
// Returns nil, nil if neither exists.
func (p *IBMCloudProvider) resolveSnapshot(ctx context.Context, ref string) (*provider.Snapshot, error) {
	if ref == "" {
		return nil, fmt.Errorf("%w: empty snapshot ID", caaProvider.ErrInvalidParameters)
	}
	snap, err := p.findSnapshotByName(ctx, ref)
	if err != nil || snap != nil {
		return snap, err
	}
	// The SDK reports every GetSnapshot failure as "snapshot ID not found";
	// only the embedded backend error distinguishes a real absence.
	snap, err = p.session.GetSnapshot(ref)
	if err != nil {
		var message providerError.Message
		if isNotFound(err) && errors.As(err, &message) && strings.Contains(strings.ToLower(message.BackendError), "not_found") {
			return nil, nil
		}
		return nil, fmt.Errorf("looking up IBM snapshot %s: %w", ref, err)
	}
	return snap, nil
}

// findSnapshotByName uses the server-side exact-name filter. Returns nil, nil if absent.
func (p *IBMCloudProvider) findSnapshotByName(ctx context.Context, name string) (*provider.Snapshot, error) {
	snaps, err := p.listSnapshots(ctx, map[string]string{"name": name})
	if err != nil {
		return nil, err
	}
	if len(snaps) > 1 {
		return nil, fmt.Errorf("%w: ambiguous snapshot name %s", caaProvider.ErrVolumeAlreadyExists, name)
	}
	if len(snaps) == 0 {
		return nil, nil
	}
	return snaps[0], nil
}

func (p *IBMCloudProvider) listSnapshots(ctx context.Context, filters map[string]string) ([]*provider.Snapshot, error) {
	var snaps []*provider.Snapshot
	seen := map[string]bool{}
	for start := ""; ; {
		if seen[start] {
			return nil, fmt.Errorf("IBM snapshot list returned a repeated pagination token")
		}
		seen[start] = true
		page, err := p.session.ListSnapshots(100, start, filters)
		if err != nil {
			return nil, fmt.Errorf("listing IBM snapshots: %w", err)
		}
		if page == nil {
			return nil, fmt.Errorf("IBM snapshot list returned a nil response")
		}
		for _, snap := range page.Snapshots {
			if snap != nil {
				snaps = append(snaps, snap)
			}
		}
		if page.Next == "" {
			return snaps, nil
		}
		start = page.Next
	}
}

// sourceVolumeName maps a native volume ID to the CSI volume name recorded in
// its ownership tag. It returns "" if the volume is gone or not owned by this
// driver. Results are memoised in cache.
func (p *IBMCloudProvider) sourceVolumeName(nativeID string, cache map[string]string) (string, error) {
	if nativeID == "" {
		return "", nil
	}
	if name, ok := cache[nativeID]; ok {
		return name, nil
	}
	name := ""
	vol, err := p.session.GetVolume(nativeID)
	switch {
	case err == nil && vol != nil:
		for _, tag := range vol.Tags {
			if strings.HasPrefix(tag, ownershipTagPrefix) {
				if name != "" {
					name = "" // ambiguous ownership
					break
				}
				name = strings.TrimPrefix(tag, ownershipTagPrefix)
			}
		}
	case err != nil && !isNotFound(err):
		return "", fmt.Errorf("resolving snapshot source volume %s: %w", nativeID, err)
	}
	cache[nativeID] = name
	return name, nil
}

func snapshotInfo(snap *provider.Snapshot, sourceVolumeName string) *caaProvider.SnapshotInfo {
	var created int64
	if !snap.SnapshotCreationTime.IsZero() {
		created = snap.SnapshotCreationTime.Unix()
	}
	return &caaProvider.SnapshotInfo{
		SnapshotID:     snap.SnapshotID,
		SourceVolumeID: sourceVolumeName,
		SizeBytes:      snap.SnapshotSize,
		CreationTime:   created,
		ReadyToUse:     snap.ReadyToUse,
	}
}
