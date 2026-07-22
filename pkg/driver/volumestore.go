// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"sync"

	provider "github.com/confidential-devhub/caa-csi-block-driver/pkg/provider"
)

var vsLogger = log.New(log.Writer(), "[caa-csi/store] ", log.LstdFlags|log.Lmsgprefix)

const (
	defaultVolumeStoreDir = "/var/lib/caa-csi-block/volumes"
	manifestFileName      = "_manifest.json"
	bootstrapParamsEnv    = "CSI_BOOTSTRAP_PARAMS_FILE"
)

type volumeRecord struct {
	VolumeID      string            `json:"volumeID"`
	Provider      string            `json:"provider"`
	Path          string            `json:"path"`
	CapacityBytes int64             `json:"capacityBytes,omitempty"`
	Params        map[string]string `json:"params"`
}

// volumeManifest is a lightweight params backup used when individual volume
// records are missing but the store directory (or a mounted bootstrap file)
// still has provider configuration.
type volumeManifest struct {
	Params    map[string]string `json:"params"`
	VolumeIDs []string          `json:"volumeIDs"`
}

type volumeStore struct {
	mu  sync.Mutex
	dir string
}

func newVolumeStore() *volumeStore {
	dir := os.Getenv("CSI_VOLUME_STORE_DIR")
	if dir == "" {
		dir = defaultVolumeStoreDir
	}
	os.MkdirAll(dir, 0700)
	return &volumeStore{dir: dir}
}

func (vs *volumeStore) manifestPath() string {
	return filepath.Join(vs.dir, manifestFileName)
}

// BootstrapParams resolves cloud provider parameters for recovery and
// fallback operations when a specific volume record may be missing.
//
// Priority:
//  1. Params from any surviving volume record
//  2. Params from _manifest.json in the volume store
//  3. JSON file pointed to by CSI_BOOTSTRAP_PARAMS_FILE
//  4. Well-known environment variables (CSI_CLOUD_PROVIDER, etc.)
func (vs *volumeStore) BootstrapParams() map[string]string {
	if params := vs.AnyParams(); params != nil {
		return params
	}
	if params := vs.loadManifestParams(); params != nil {
		return params
	}
	if params := loadBootstrapParamsFile(); params != nil {
		return params
	}
	return paramsFromEnv()
}

// RecoverFromCloud rebuilds missing local volume records from cloud-tagged
// volumes. Called at startup and before operations that need a local record.
func (vs *volumeStore) RecoverFromCloud(params map[string]string) error {
	if params == nil || params["cloudProvider"] == "" {
		vsLogger.Printf("No cloud provider params available, skipping cloud recovery")
		return nil
	}

	p, err := provider.NewBlockVolumeProvider(params)
	if err != nil {
		return fmt.Errorf("creating provider for recovery: %w", err)
	}

	lister, ok := p.(provider.VolumeRecoverer)
	if !ok {
		vsLogger.Printf("Provider %q does not support VolumeRecoverer, skipping", params["cloudProvider"])
		return nil
	}

	vols, err := lister.ListManagedVolumes()
	if err != nil {
		return fmt.Errorf("listing managed volumes from cloud: %w", err)
	}

	paramsCopy := maps.Clone(params)

	vs.mu.Lock()
	defer vs.mu.Unlock()

	recovered := 0
	for _, vol := range vols {
		path := filepath.Join(vs.dir, vol.VolumeID+".json")
		if _, err := os.Stat(path); err == nil {
			continue
		}

		rec := &volumeRecord{
			VolumeID:      vol.VolumeID,
			Provider:      vol.Provider,
			Path:          vol.Path,
			CapacityBytes: vol.SizeBytes,
			Params:        paramsCopy,
		}
		data, err := json.Marshal(rec)
		if err != nil {
			vsLogger.Printf("failed to marshal recovered volume %s: %v", vol.VolumeID, err)
			continue
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			vsLogger.Printf("failed to write recovered volume %s: %v", vol.VolumeID, err)
			continue
		}
		recovered++
		vsLogger.Printf("Recovered volume %s from cloud (path=%s)", vol.VolumeID, vol.Path)
	}

	if recovered > 0 {
		vsLogger.Printf("Recovered %d volume(s) from cloud provider", recovered)
		// Refresh manifest under the same lock so VolumeIDs stay accurate.
		if err := vs.writeManifestLocked(); err != nil {
			vsLogger.Printf("failed to write volume manifest after recovery: %v", err)
		}
	}
	return nil
}

// Exists checks whether a volume record exists in the store.
// Returns (false, nil) if the file simply doesn't exist, or (false, err)
// if the check failed due to a permission or I/O error.
func (vs *volumeStore) Exists(volumeID string) (bool, error) {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	_, err := os.Stat(filepath.Join(vs.dir, volumeID+".json"))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (vs *volumeStore) Save(rec *volumeRecord) error {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("failed to marshal volume record: %w", err)
	}
	if err := os.WriteFile(filepath.Join(vs.dir, rec.VolumeID+".json"), data, 0600); err != nil {
		return err
	}
	return vs.writeManifestLocked()
}

func (vs *volumeStore) Load(volumeID string) (*volumeRecord, error) {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	data, err := os.ReadFile(filepath.Join(vs.dir, volumeID+".json"))
	if err != nil {
		return nil, err
	}
	var rec volumeRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("failed to unmarshal volume record: %w", err)
	}
	return &rec, nil
}

func (vs *volumeStore) Delete(volumeID string) {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	os.Remove(filepath.Join(vs.dir, volumeID+".json"))
	if err := vs.writeManifestLocked(); err != nil {
		vsLogger.Printf("failed to write volume manifest after delete: %v", err)
	}
}

// AnyParams returns the Params map from any volume in the store, or nil.
func (vs *volumeStore) AnyParams() map[string]string {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	entries, err := os.ReadDir(vs.dir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" || e.Name() == manifestFileName {
			continue
		}
		data, err := os.ReadFile(filepath.Join(vs.dir, e.Name()))
		if err != nil {
			continue
		}
		var rec volumeRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			continue
		}
		if len(rec.Params) > 0 && rec.Params["cloudProvider"] != "" {
			return maps.Clone(rec.Params)
		}
	}
	return nil
}

// listAll returns all volume records. Caller must hold vs.mu.
func (vs *volumeStore) listAll() ([]*volumeRecord, error) {
	entries, err := os.ReadDir(vs.dir)
	if err != nil {
		return nil, err
	}
	var recs []*volumeRecord
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" || e.Name() == manifestFileName {
			continue
		}
		data, err := os.ReadFile(filepath.Join(vs.dir, e.Name()))
		if err != nil {
			continue
		}
		var rec volumeRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			continue
		}
		recs = append(recs, &rec)
	}
	return recs, nil
}

// writeManifestLocked updates or removes _manifest.json. Caller must hold vs.mu.
func (vs *volumeStore) writeManifestLocked() error {
	recs, err := vs.listAll()
	if err != nil {
		return err
	}

	path := vs.manifestPath()
	if len(recs) == 0 {
		// Keep a params-only manifest if one already exists so empty-store
		// recovery can still bootstrap after the last volume is deleted.
		existing, loadErr := vs.readManifestLocked()
		if loadErr != nil || existing == nil || len(existing.Params) == 0 || existing.Params["cloudProvider"] == "" {
			_ = os.Remove(path)
			return nil
		}
		existing.VolumeIDs = nil
		data, err := json.Marshal(existing)
		if err != nil {
			return err
		}
		return os.WriteFile(path, data, 0600)
	}

	m := volumeManifest{
		Params:    maps.Clone(recs[0].Params),
		VolumeIDs: make([]string, 0, len(recs)),
	}
	for _, r := range recs {
		m.VolumeIDs = append(m.VolumeIDs, r.VolumeID)
	}

	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func (vs *volumeStore) readManifestLocked() (*volumeManifest, error) {
	data, err := os.ReadFile(vs.manifestPath())
	if err != nil {
		return nil, err
	}
	var m volumeManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func (vs *volumeStore) loadManifestParams() map[string]string {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	m, err := vs.readManifestLocked()
	if err != nil || m == nil || len(m.Params) == 0 || m.Params["cloudProvider"] == "" {
		return nil
	}
	vsLogger.Printf("Loaded bootstrap params from %s", manifestFileName)
	return maps.Clone(m.Params)
}

func loadBootstrapParamsFile() map[string]string {
	path := os.Getenv(bootstrapParamsEnv)
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		vsLogger.Printf("failed to read %s=%s: %v", bootstrapParamsEnv, path, err)
		return nil
	}
	var params map[string]string
	if err := json.Unmarshal(data, &params); err != nil {
		vsLogger.Printf("failed to parse bootstrap params file %s: %v", path, err)
		return nil
	}
	if params["cloudProvider"] == "" {
		vsLogger.Printf("bootstrap params file %s missing cloudProvider", path)
		return nil
	}
	vsLogger.Printf("Loaded bootstrap params from %s", path)
	return params
}

// paramsFromEnv builds StorageClass-compatible params from environment variables
// so recovery/delete can work on a node whose hostPath store is empty.
func paramsFromEnv() map[string]string {
	cloudProvider := firstNonEmpty(
		os.Getenv("CSI_CLOUD_PROVIDER"),
		os.Getenv("CLOUD_PROVIDER"),
	)
	if cloudProvider == "" {
		return nil
	}

	params := map[string]string{"cloudProvider": cloudProvider}

	switch cloudProvider {
	case "aws":
		setIfNonEmpty(params, "awsRegion", firstNonEmpty(os.Getenv("CSI_AWS_REGION"), os.Getenv("AWS_REGION")))
		setIfNonEmpty(params, "awsAvailabilityZone", firstNonEmpty(os.Getenv("CSI_AWS_AVAILABILITY_ZONE"), os.Getenv("AWS_AVAILABILITY_ZONE")))
		setIfNonEmpty(params, "awsVolumeType", os.Getenv("CSI_AWS_VOLUME_TYPE"))
		setIfNonEmpty(params, "awsAccessKeyId", os.Getenv("CSI_AWS_ACCESS_KEY_ID"))
		setIfNonEmpty(params, "awsSecretKey", os.Getenv("CSI_AWS_SECRET_KEY"))
		setIfNonEmpty(params, "awsKmsKeyId", os.Getenv("CSI_AWS_KMS_KEY_ID"))
	case "azure":
		setIfNonEmpty(params, "azureSubscriptionId", firstNonEmpty(os.Getenv("CSI_AZURE_SUBSCRIPTION_ID"), os.Getenv("AZURE_SUBSCRIPTION_ID")))
		setIfNonEmpty(params, "azureResourceGroup", firstNonEmpty(os.Getenv("CSI_AZURE_RESOURCE_GROUP"), os.Getenv("AZURE_RESOURCE_GROUP")))
		setIfNonEmpty(params, "azureLocation", firstNonEmpty(os.Getenv("CSI_AZURE_LOCATION"), os.Getenv("AZURE_LOCATION")))
		setIfNonEmpty(params, "azureDiskSKU", os.Getenv("CSI_AZURE_DISK_SKU"))
		setIfNonEmpty(params, "azureDiskEncryptionSetId", os.Getenv("CSI_AZURE_DISK_ENCRYPTION_SET_ID"))
	case "libvirt":
		setIfNonEmpty(params, "cloudProviderVolumePath", firstNonEmpty(os.Getenv("CSI_LIBVIRT_VOLUME_PATH"), os.Getenv("CLOUD_PROVIDER_VOLUME_PATH")))
	}

	vsLogger.Printf("Loaded bootstrap params from environment (provider=%s)", cloudProvider)
	return params
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func setIfNonEmpty(params map[string]string, key, value string) {
	if value != "" {
		params[key] = value
	}
}
