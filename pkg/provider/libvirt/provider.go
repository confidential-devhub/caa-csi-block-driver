// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package libvirt

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	provider "github.com/confidential-devhub/caa-csi-block-driver/pkg/provider"
)

var _ provider.VolumeCloner = (*LibvirtProvider)(nil)
var _ provider.VolumeExpander = (*LibvirtProvider)(nil)
var _ provider.VolumeRecoverer = (*LibvirtProvider)(nil)
var _ provider.VolumeSnapshotter = (*LibvirtProvider)(nil)

var logger = log.New(log.Writer(), "[caa-csi/libvirt] ", log.LstdFlags|log.Lmsgprefix)

func init() {
	provider.RegisterProvider("libvirt", func(params map[string]string) (provider.BlockVolumeProvider, error) {
		return NewLibvirtProvider(params)
	})
}

const (
	defaultFsType    = "ext4"
	volumeFilePrefix = "csi-vol-"
	snapFilePrefix   = "csi-snap-"
	rawSuffix        = ".raw"
)

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

func sanitizeID(id string) string {
	return strings.ReplaceAll(strings.ReplaceAll(id, "/", "-"), "..", "-")
}

func (p *LibvirtProvider) volumePath(volumeID string) string {
	return filepath.Join(p.config.PoolPath, volumeFilePrefix+sanitizeID(volumeID)+rawSuffix)
}

func (p *LibvirtProvider) snapshotPath(snapshotID string) string {
	return filepath.Join(p.config.PoolPath, snapFilePrefix+sanitizeID(snapshotID)+rawSuffix)
}

func (p *LibvirtProvider) snapshotMetaPath(snapshotID string) string {
	return filepath.Join(p.config.PoolPath, snapFilePrefix+sanitizeID(snapshotID)+".meta")
}

func (p *LibvirtProvider) writeSnapshotSource(snapshotID, volumeID string) {
	if err := os.WriteFile(p.snapshotMetaPath(snapshotID), []byte(volumeID), 0600); err != nil {
		logger.Printf("WARNING: failed to write snapshot meta for %s: %v", snapshotID, err)
	}
}

func (p *LibvirtProvider) readSnapshotSource(snapshotID string) string {
	data, err := os.ReadFile(p.snapshotMetaPath(snapshotID))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func (p *LibvirtProvider) CreateVolume(_ context.Context, volumeID string, sizeBytes int64) (*provider.VolumeInfo, error) {
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

func (p *LibvirtProvider) DeleteVolume(_ context.Context, volumeID string) error {
	volPath := p.volumePath(volumeID)

	if err := os.Remove(volPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete volume file %s: %w", volPath, err)
	}

	logger.Printf("Deleted volume %s at %s", volumeID, volPath)
	return nil
}

func (p *LibvirtProvider) GetVolumeInfo(_ context.Context, volumeID string) (*provider.VolumeInfo, error) {
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

func (p *LibvirtProvider) VolumeExists(_ context.Context, volumeID string) (bool, error) {
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

func (p *LibvirtProvider) ExpandVolume(_ context.Context, volumeID string, newSizeBytes int64) error {
	volPath := p.volumePath(volumeID)
	if _, err := os.Stat(volPath); err != nil {
		return fmt.Errorf("volume %s not found at %s: %w", volumeID, volPath, err)
	}
	if err := os.Truncate(volPath, newSizeBytes); err != nil {
		return fmt.Errorf("failed to expand volume %s to %d bytes: %w", volumeID, newSizeBytes, err)
	}
	logger.Printf("Expanded volume %s to %d bytes", volumeID, newSizeBytes)
	return nil
}

func (p *LibvirtProvider) CreateVolumeFromSnapshot(ctx context.Context, volumeID, snapshotID string, sizeBytes int64) (*provider.VolumeInfo, error) {
	snapPath := p.snapshotPath(snapshotID)
	if _, err := os.Stat(snapPath); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("snapshot %s not found at %s", snapshotID, snapPath)
		}
		return nil, fmt.Errorf("failed to access snapshot %s: %w", snapshotID, err)
	}

	dstPath := p.volumePath(volumeID)
	if _, err := os.Stat(dstPath); err == nil {
		logger.Printf("Volume %s already exists at %s, reusing", volumeID, dstPath)
		return p.GetVolumeInfo(ctx, volumeID)
	}

	srcFile, err := os.Open(snapPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open snapshot %s: %w", snapPath, err)
	}
	defer srcFile.Close()

	dstFile, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		return nil, fmt.Errorf("failed to create volume %s from snapshot: %w", dstPath, err)
	}

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		dstFile.Close()
		os.Remove(dstPath)
		return nil, fmt.Errorf("failed to restore snapshot data: %w", err)
	}
	if err := dstFile.Close(); err != nil {
		os.Remove(dstPath)
		return nil, fmt.Errorf("failed to finalize restored volume %s: %w", dstPath, err)
	}

	snapInfo, err := os.Stat(snapPath)
	if err != nil {
		return nil, fmt.Errorf("failed to stat snapshot after restore: %w", err)
	}
	if sizeBytes > snapInfo.Size() {
		if err := os.Truncate(dstPath, sizeBytes); err != nil {
			return nil, fmt.Errorf("failed to resize restored volume: %w", err)
		}
	}

	logger.Printf("Created volume %s from snapshot %s", volumeID, snapshotID)
	return &provider.VolumeInfo{
		VolumeID:  volumeID,
		Path:      dstPath,
		SizeBytes: sizeBytes,
		Provider:  "libvirt",
		Metadata: map[string]string{
			"cloud-volume-path": dstPath,
			"cloud-provider":    "libvirt",
			"source-snapshot":   snapshotID,
		},
	}, nil
}

// CreateVolumeFromVolume clones a volume via file copy. Unlike AWS (point-in-time
// snapshot) or Azure (atomic server-side copy), this performs a userspace io.Copy
// which is NOT atomic — cloning a source that is actively being written may
// produce a crash-inconsistent image. Callers should ensure the source PVC is
// not mounted read-write during the clone operation.
func (p *LibvirtProvider) CreateVolumeFromVolume(ctx context.Context, volumeID, sourceVolumeID string, sizeBytes int64) (*provider.VolumeInfo, error) {
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
		return p.GetVolumeInfo(ctx, volumeID)
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

func (p *LibvirtProvider) CreateSnapshot(_ context.Context, volumeID, snapshotID string) (*provider.SnapshotInfo, error) {
	srcPath := p.volumePath(volumeID)
	srcInfo, err := os.Stat(srcPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("source volume %s not found at %s", volumeID, srcPath)
		}
		return nil, fmt.Errorf("failed to access source volume %s: %w", volumeID, err)
	}

	snapPath := p.snapshotPath(snapshotID)
	if snapInfo, err := os.Stat(snapPath); err == nil {
		logger.Printf("Snapshot %s already exists at %s, reusing", snapshotID, snapPath)
		return &provider.SnapshotInfo{
			SnapshotID:     snapshotID,
			SourceVolumeID: volumeID,
			SizeBytes:      snapInfo.Size(),
			CreationTime:   snapInfo.ModTime().Unix(),
			ReadyToUse:     true,
		}, nil
	}

	srcFile, err := os.Open(srcPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open volume %s for snapshot: %w", srcPath, err)
	}
	defer srcFile.Close()

	dstFile, err := os.OpenFile(snapPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		return nil, fmt.Errorf("failed to create snapshot file %s: %w", snapPath, err)
	}

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		dstFile.Close()
		os.Remove(snapPath)
		return nil, fmt.Errorf("failed to copy volume data for snapshot: %w", err)
	}
	if err := dstFile.Close(); err != nil {
		os.Remove(snapPath)
		return nil, fmt.Errorf("failed to finalize snapshot %s: %w", snapPath, err)
	}

	p.writeSnapshotSource(snapshotID, volumeID)

	logger.Printf("Created snapshot %s from volume %s", snapshotID, volumeID)
	return &provider.SnapshotInfo{
		SnapshotID:     snapshotID,
		SourceVolumeID: volumeID,
		SizeBytes:      srcInfo.Size(),
		CreationTime:   time.Now().Unix(),
		ReadyToUse:     true,
	}, nil
}

func (p *LibvirtProvider) DeleteSnapshot(_ context.Context, snapshotID string) error {
	snapPath := p.snapshotPath(snapshotID)
	if err := os.Remove(snapPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete snapshot file %s: %w", snapPath, err)
	}
	os.Remove(p.snapshotMetaPath(snapshotID))
	logger.Printf("Deleted snapshot %s", snapshotID)
	return nil
}

func (p *LibvirtProvider) ListSnapshots(_ context.Context, volumeID string) ([]*provider.SnapshotInfo, error) {
	entries, err := os.ReadDir(p.config.PoolPath)
	if err != nil {
		return nil, fmt.Errorf("failed to list snapshot directory: %w", err)
	}

	var snaps []*provider.SnapshotInfo
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, snapFilePrefix) || !strings.HasSuffix(name, rawSuffix) {
			continue
		}
		snapID := strings.TrimPrefix(name, snapFilePrefix)
		snapID = strings.TrimSuffix(snapID, rawSuffix)

		info, err := entry.Info()
		if err != nil {
			continue
		}

		sourceVol := p.readSnapshotSource(snapID)
		if volumeID != "" && sourceVol != volumeID {
			continue
		}

		snaps = append(snaps, &provider.SnapshotInfo{
			SnapshotID:     snapID,
			SourceVolumeID: sourceVol,
			SizeBytes:      info.Size(),
			CreationTime:   info.ModTime().Unix(),
			ReadyToUse:     true,
		})
	}
	return snaps, nil
}

func (p *LibvirtProvider) FindSnapshot(_ context.Context, snapshotID string) (*provider.SnapshotInfo, error) {
	snapPath := p.snapshotPath(snapshotID)
	info, err := os.Stat(snapPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to stat snapshot %s: %w", snapshotID, err)
	}

	return &provider.SnapshotInfo{
		SnapshotID:     snapshotID,
		SourceVolumeID: p.readSnapshotSource(snapshotID),
		SizeBytes:      info.Size(),
		CreationTime:   info.ModTime().Unix(),
		ReadyToUse:     true,
	}, nil
}

func (p *LibvirtProvider) ListManagedVolumes(_ context.Context) ([]*provider.VolumeInfo, error) {
	entries, err := os.ReadDir(p.config.PoolPath)
	if err != nil {
		return nil, fmt.Errorf("failed to list volume directory: %w", err)
	}

	var vols []*provider.VolumeInfo
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, volumeFilePrefix) || !strings.HasSuffix(name, rawSuffix) {
			continue
		}
		volID := strings.TrimPrefix(name, volumeFilePrefix)
		volID = strings.TrimSuffix(volID, rawSuffix)

		info, err := entry.Info()
		if err != nil {
			continue
		}

		volPath := filepath.Join(p.config.PoolPath, name)
		vols = append(vols, &provider.VolumeInfo{
			VolumeID:  volID,
			Path:      volPath,
			SizeBytes: info.Size(),
			Provider:  "libvirt",
			Metadata: map[string]string{
				"cloud-volume-path": volPath,
				"cloud-provider":    "libvirt",
			},
		})
	}
	return vols, nil
}
