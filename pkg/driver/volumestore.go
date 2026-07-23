// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

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
// Short-circuits after reading the first valid record to avoid O(n) cost.
func (vs *volumeStore) AnyParams() map[string]string {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	entries, err := os.ReadDir(vs.dir)
	if err != nil {
		return nil
	}
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
		return rec.Params
	}
	return nil
}

