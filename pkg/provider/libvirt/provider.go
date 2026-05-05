// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package libvirt

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"

	diskfs "github.com/diskfs/go-diskfs"

	provider "github.com/confidential-devhub/caa-csi-block-driver/pkg/provider"
)

var logger = log.New(log.Writer(), "[caa-csi/libvirt] ", log.LstdFlags|log.Lmsgprefix)

func init() {
	provider.RegisterProvider("libvirt", func(params map[string]string) (provider.BlockVolumeProvider, error) {
		return NewLibvirtProvider(params)
	})
}

const defaultFsType = "ext4"

type Config struct {
	PoolPath string
}

type LibvirtProvider struct {
	config Config
}

func NewLibvirtProvider(params map[string]string) (*LibvirtProvider, error) {
	poolPath := params["cloudProviderVolumePath"]
	if poolPath == "" {
		return nil, fmt.Errorf("cloudProviderVolumePath is required for libvirt provider")
	}

	if info, err := os.Stat(poolPath); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("pool path %s does not exist or is not a directory", poolPath)
	}

	return &LibvirtProvider{
		config: Config{PoolPath: poolPath},
	}, nil
}

func (p *LibvirtProvider) volumePath(volumeID string) string {
	return filepath.Join(p.config.PoolPath, fmt.Sprintf("csi-vol-%s.raw", volumeID))
}

func (p *LibvirtProvider) CreateVolume(volumeID string, sizeBytes int64) (*provider.VolumeInfo, error) {
	volPath := p.volumePath(volumeID)

	if _, err := os.Stat(volPath); os.IsNotExist(err) {
		logger.Printf("Creating volume %s at %s (%d bytes)", volumeID, volPath, sizeBytes)

		if _, err := diskfs.Create(volPath, sizeBytes, diskfs.Raw, diskfs.SectorSizeDefault); err != nil {
			return nil, fmt.Errorf("failed to create raw disk at %s: %w", volPath, err)
		}

		mkfsCmd := exec.Command("mkfs.ext4", "-F", "-m0", volPath)
		if out, err := mkfsCmd.CombinedOutput(); err != nil {
			os.Remove(volPath)
			return nil, fmt.Errorf("failed to format %s as %s: %w (output: %s)", volPath, defaultFsType, err, string(out))
		}
		logger.Printf("Formatted %s as %s", volPath, defaultFsType)
	} else {
		logger.Printf("Volume already exists at %s, reusing (preserving data)", volPath)
	}

	return &provider.VolumeInfo{
		VolumeID:  volumeID,
		Path:      volPath,
		SizeBytes: sizeBytes,
		Provider:  "libvirt",
		Metadata: map[string]string{
			"cloud-volume-path": volPath,
			"cloud-provider":    "libvirt",
		},
	}, nil
}

func (p *LibvirtProvider) DeleteVolume(volumeID string) error {
	volPath := p.volumePath(volumeID)

	if err := os.Remove(volPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete volume file %s: %w", volPath, err)
	}

	logger.Printf("Deleted volume %s at %s", volumeID, volPath)
	return nil
}

func (p *LibvirtProvider) GetVolumeInfo(volumeID string) (*provider.VolumeInfo, error) {
	volPath := p.volumePath(volumeID)

	info, err := os.Stat(volPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("volume %s not found at %s", volumeID, volPath)
		}
		return nil, fmt.Errorf("failed to stat volume %s: %w", volPath, err)
	}

	return &provider.VolumeInfo{
		VolumeID:  volumeID,
		Path:      volPath,
		SizeBytes: info.Size(),
		Provider:  "libvirt",
		Metadata: map[string]string{
			"cloud-volume-path": volPath,
			"cloud-provider":    "libvirt",
		},
	}, nil
}

func (p *LibvirtProvider) VolumeExists(volumeID string) (bool, error) {
	volPath := p.volumePath(volumeID)
	_, err := os.Stat(volPath)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("failed to check volume %s: %w", volPath, err)
}
