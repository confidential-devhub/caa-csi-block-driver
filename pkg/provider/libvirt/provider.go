// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package libvirt

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"

	provider "github.com/confidential-devhub/caa-csi-block-driver/pkg/provider"
)

var _ provider.VolumeCloner = (*LibvirtProvider)(nil)

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

		f, err := os.Create(volPath)
		if err != nil {
			return nil, fmt.Errorf("failed to create raw disk at %s: %w", volPath, err)
		}
		if err := f.Truncate(sizeBytes); err != nil {
			f.Close()
			os.Remove(volPath)
			return nil, fmt.Errorf("failed to allocate %d bytes for %s: %w", sizeBytes, volPath, err)
		}
		f.Close()

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

func (p *LibvirtProvider) CreateVolumeFromSnapshot(_, snapshotID string, _ int64) (*provider.VolumeInfo, error) {
	return nil, fmt.Errorf("libvirt provider does not support snapshots (snapshot %s)", snapshotID)
}

// CreateVolumeFromVolume clones a volume via file copy. Unlike AWS (point-in-time
// snapshot) or Azure (atomic server-side copy), this performs a userspace io.Copy
// which is NOT atomic — cloning a source that is actively being written may
// produce a crash-inconsistent image. Callers should ensure the source PVC is
// not mounted read-write during the clone operation.
func (p *LibvirtProvider) CreateVolumeFromVolume(volumeID, sourceVolumeID string, sizeBytes int64) (*provider.VolumeInfo, error) {
	srcPath := p.volumePath(sourceVolumeID)
	if _, err := os.Stat(srcPath); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("source volume %s not found at %s", sourceVolumeID, srcPath)
		}
		return nil, fmt.Errorf("failed to access source volume %s: %w", sourceVolumeID, err)
	}

	dstPath := p.volumePath(volumeID)
	if _, err := os.Stat(dstPath); err == nil {
		logger.Printf("Volume %s already exists at %s, reusing", volumeID, dstPath)
		return p.GetVolumeInfo(volumeID)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to check destination volume %s: %w", dstPath, err)
	}

	logger.Printf("WARNING: cloning %s via file copy — source should not be actively written during this operation", sourceVolumeID)

	srcFile, err := os.Open(srcPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open source volume %s: %w", srcPath, err)
	}
	defer srcFile.Close()

	dstFile, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		return nil, fmt.Errorf("failed to create cloned volume %s: %w", dstPath, err)
	}

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		dstFile.Close()
		os.Remove(dstPath)
		return nil, fmt.Errorf("failed to copy volume data: %w", err)
	}
	if err := dstFile.Close(); err != nil {
		os.Remove(dstPath)
		return nil, fmt.Errorf("failed to finalize cloned volume %s: %w", dstPath, err)
	}

	srcInfo, err := os.Stat(srcPath)
	if err != nil {
		return nil, fmt.Errorf("failed to stat source volume after copy: %w", err)
	}
	if sizeBytes > srcInfo.Size() {
		if err := os.Truncate(dstPath, sizeBytes); err != nil {
			return nil, fmt.Errorf("failed to resize cloned volume: %w", err)
		}
		// Grow the filesystem to fill the expanded space. Without this, the ext4
		// FS remains at the source's original size and the extra space is
		// unreachable — Kubernetes won't trigger NodeExpandVolume because the
		// reported capacity already matches the PVC request.
		if out, err := exec.Command("resize2fs", dstPath).CombinedOutput(); err != nil {
			os.Remove(dstPath)
			return nil, fmt.Errorf("failed to grow filesystem on cloned volume %s: %w (output: %s)", dstPath, err, string(out))
		}
	}

	logger.Printf("Cloned volume %s from %s", volumeID, sourceVolumeID)
	return &provider.VolumeInfo{
		VolumeID:  volumeID,
		Path:      dstPath,
		SizeBytes: sizeBytes,
		Provider:  "libvirt",
		Metadata: map[string]string{
			"cloud-volume-path": dstPath,
			"cloud-provider":    "libvirt",
		},
	}, nil
}
