// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	provider "github.com/confidential-devhub/caa-csi-block-driver/pkg/provider"
)

var vsLogger = log.New(log.Writer(), "[caa-csi/store] ", log.LstdFlags|log.Lmsgprefix)

const defaultVolumeStoreDir = "/var/lib/caa-csi-block/volumes"

type volumeRecord struct {
	VolumeID      string            `json:"volumeID"`
	Provider      string            `json:"provider"`
	Path          string            `json:"path"`
	CapacityBytes int64             `json:"capacityBytes,omitempty"`
	Params        map[string]string `json:"params"`
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

// RecoverFromCloud queries the cloud provider for volumes tagged with our
// CSI tag and rebuilds any missing local volume records. This handles the
// case where the CSI driver pod is rescheduled and loses its local state.
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
		vsLogger.Printf("Provider %q does not support volume recovery (VolumeRecoverer interface), skipping", params["cloudProvider"])
		return nil
	}

	vols, err := lister.ListManagedVolumes()
	if err != nil {
		return fmt.Errorf("listing managed volumes from cloud: %w", err)
	}

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
			Params:        params,
		}
		data, err := json.Marshal(rec)
		if err != nil {
			vsLogger.Printf("WARNING: failed to marshal recovered volume %s: %v", vol.VolumeID, err)
			continue
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			vsLogger.Printf("WARNING: failed to write recovered volume %s: %v", vol.VolumeID, err)
			continue
		}
		recovered++
		vsLogger.Printf("Recovered volume %s from cloud (path=%s)", vol.VolumeID, vol.Path)
	}

	if recovered > 0 {
		vsLogger.Printf("Recovered %d volume(s) from cloud provider", recovered)
	}
	return nil
}

func (vs *volumeStore) Exists(volumeID string) bool {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	_, err := os.Stat(filepath.Join(vs.dir, volumeID+".json"))
	return err == nil
}

func (vs *volumeStore) Save(rec *volumeRecord) error {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("failed to marshal volume record: %w", err)
	}
	return os.WriteFile(filepath.Join(vs.dir, rec.VolumeID+".json"), data, 0600)
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
}

// AnyParams returns the Params map from any volume in the store, or nil.
func (vs *volumeStore) AnyParams() map[string]string {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	recs, err := vs.listAll()
	if err != nil || len(recs) == 0 {
		return nil
	}
	return recs[0].Params
}

// listAll returns all volume records. Caller must hold vs.mu.
func (vs *volumeStore) listAll() ([]*volumeRecord, error) {
	entries, err := os.ReadDir(vs.dir)
	if err != nil {
		return nil, err
	}
	var recs []*volumeRecord
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
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
