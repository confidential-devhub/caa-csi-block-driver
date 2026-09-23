// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package libvirt

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var ctx = context.Background()

func TestNewLibvirtProvider_Validation(t *testing.T) {
	t.Run("missing pool path", func(t *testing.T) {
		_, err := NewLibvirtProvider(map[string]string{})
		if err == nil {
			t.Fatal("expected error for missing cloudProviderVolumePath")
		}
		if !strings.Contains(err.Error(), "cloudProviderVolumePath is required") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("nonexistent pool path", func(t *testing.T) {
		_, err := NewLibvirtProvider(map[string]string{
			"cloudProviderVolumePath": "/tmp/does-not-exist-" + t.Name(),
		})
		if err == nil {
			t.Fatal("expected error for nonexistent path")
		}
		if !strings.Contains(err.Error(), "does not exist") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("pool path is a file not directory", func(t *testing.T) {
		f, err := os.CreateTemp("", "libvirt-test-file")
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
		defer os.Remove(f.Name())

		_, err = NewLibvirtProvider(map[string]string{
			"cloudProviderVolumePath": f.Name(),
		})
		if err == nil {
			t.Fatal("expected error when path is a file")
		}
	})

	t.Run("valid pool path", func(t *testing.T) {
		dir := t.TempDir()
		p, err := NewLibvirtProvider(map[string]string{
			"cloudProviderVolumePath": dir,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if p.config.PoolPath != dir {
			t.Errorf("PoolPath = %q, want %q", p.config.PoolPath, dir)
		}
	})
}

func TestVolumePath(t *testing.T) {
	p := &LibvirtProvider{config: Config{PoolPath: "/data/pool"}}

	got := p.volumePath("pvc-abc123")
	want := "/data/pool/csi-vol-pvc-abc123.raw"
	if got != want {
		t.Errorf("volumePath(%q) = %q, want %q", "pvc-abc123", got, want)
	}
}

func newTestProvider(t *testing.T) *LibvirtProvider {
	t.Helper()
	return &LibvirtProvider{config: Config{PoolPath: t.TempDir()}}
}

func TestDeleteVolume(t *testing.T) {
	t.Run("deletes existing file", func(t *testing.T) {
		p := newTestProvider(t)
		volPath := p.volumePath("vol-1")
		if err := os.WriteFile(volPath, []byte("data"), 0600); err != nil {
			t.Fatal(err)
		}

		if err := p.DeleteVolume(ctx, "vol-1"); err != nil {
			t.Fatalf("DeleteVolume failed: %v", err)
		}

		if _, err := os.Stat(volPath); !os.IsNotExist(err) {
			t.Error("volume file should be gone after delete")
		}
	})

	t.Run("idempotent on missing volume", func(t *testing.T) {
		p := newTestProvider(t)
		if err := p.DeleteVolume(ctx, "does-not-exist"); err != nil {
			t.Fatalf("DeleteVolume on missing volume should not error: %v", err)
		}
	})
}

func TestGetVolumeInfo(t *testing.T) {
	t.Run("returns info for existing volume", func(t *testing.T) {
		p := newTestProvider(t)
		volPath := p.volumePath("vol-1")
		data := make([]byte, 1024)
		if err := os.WriteFile(volPath, data, 0600); err != nil {
			t.Fatal(err)
		}

		info, err := p.GetVolumeInfo(ctx, "vol-1")
		if err != nil {
			t.Fatalf("GetVolumeInfo failed: %v", err)
		}
		if info.VolumeID != "vol-1" {
			t.Errorf("VolumeID = %q, want %q", info.VolumeID, "vol-1")
		}
		if info.Path != volPath {
			t.Errorf("Path = %q, want %q", info.Path, volPath)
		}
		if info.SizeBytes != int64(len(data)) {
			t.Errorf("SizeBytes = %d, want %d", info.SizeBytes, len(data))
		}
		if info.Provider != "libvirt" {
			t.Errorf("Provider = %q, want %q", info.Provider, "libvirt")
		}
	})

	t.Run("errors on missing volume", func(t *testing.T) {
		p := newTestProvider(t)
		_, err := p.GetVolumeInfo(ctx, "nonexistent")
		if err == nil {
			t.Fatal("expected error for missing volume")
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Errorf("error should mention 'not found': %v", err)
		}
	})
}

func TestVolumeExists(t *testing.T) {
	t.Run("true when file exists", func(t *testing.T) {
		p := newTestProvider(t)
		volPath := p.volumePath("vol-1")
		if err := os.WriteFile(volPath, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}

		exists, err := p.VolumeExists(ctx, "vol-1")
		if err != nil {
			t.Fatalf("VolumeExists failed: %v", err)
		}
		if !exists {
			t.Error("expected volume to exist")
		}
	})

	t.Run("false when file missing", func(t *testing.T) {
		p := newTestProvider(t)
		exists, err := p.VolumeExists(ctx, "nonexistent")
		if err != nil {
			t.Fatalf("VolumeExists failed: %v", err)
		}
		if exists {
			t.Error("expected volume to not exist")
		}
	})
}

func TestCreateVolumeFromSnapshot(t *testing.T) {
	p := newTestProvider(t)
	_, err := p.CreateVolumeFromSnapshot(ctx, "", "snap-1", 0)
	if err == nil {
		t.Fatal("expected error since libvirt doesn't support snapshots")
	}
	if !strings.Contains(err.Error(), "does not support snapshots") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCreateVolumeFromVolume(t *testing.T) {
	t.Run("errors on missing source", func(t *testing.T) {
		p := newTestProvider(t)
		_, err := p.CreateVolumeFromVolume(ctx, "dst", "nonexistent-src", 1024)
		if err == nil {
			t.Fatal("expected error for missing source")
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Errorf("error should mention 'not found': %v", err)
		}
	})

	t.Run("clones existing source", func(t *testing.T) {
		p := newTestProvider(t)
		srcPath := p.volumePath("src-vol")
		srcData := []byte("source data for clone test")
		if err := os.WriteFile(srcPath, srcData, 0600); err != nil {
			t.Fatal(err)
		}

		info, err := p.CreateVolumeFromVolume(ctx, "dst-vol", "src-vol", int64(len(srcData)))
		if err != nil {
			t.Fatalf("CreateVolumeFromVolume failed: %v", err)
		}
		if info.VolumeID != "dst-vol" {
			t.Errorf("VolumeID = %q, want %q", info.VolumeID, "dst-vol")
		}

		dstData, err := os.ReadFile(p.volumePath("dst-vol"))
		if err != nil {
			t.Fatalf("failed to read cloned volume: %v", err)
		}
		if string(dstData) != string(srcData) {
			t.Errorf("cloned data = %q, want %q", string(dstData), string(srcData))
		}
	})

	t.Run("reuses existing destination", func(t *testing.T) {
		p := newTestProvider(t)
		srcPath := p.volumePath("src")
		dstPath := p.volumePath("dst")
		if err := os.WriteFile(srcPath, []byte("src"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dstPath, []byte("existing-dst"), 0600); err != nil {
			t.Fatal(err)
		}

		info, err := p.CreateVolumeFromVolume(ctx, "dst", "src", 100)
		if err != nil {
			t.Fatalf("expected reuse, got error: %v", err)
		}
		if info.VolumeID != "dst" {
			t.Errorf("VolumeID = %q, want %q", info.VolumeID, "dst")
		}

		data, err := os.ReadFile(dstPath)
		if err != nil {
			t.Fatalf("failed to read destination: %v", err)
		}
		if string(data) != "existing-dst" {
			t.Error("existing destination should not be overwritten")
		}
	})
}

func TestCreateVolume_FileCreation(t *testing.T) {
	p := newTestProvider(t)
	volPath := p.volumePath("new-vol")

	_, err := p.CreateVolume(ctx, "new-vol", 1024*1024)
	// mkfs.ext4 may not be available in the test env
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			t.Skipf("skipping: mkfs.ext4 not available: %v", err)
		}
		t.Fatalf("CreateVolume failed: %v", err)
	}

	info, err := os.Stat(volPath)
	if err != nil {
		t.Fatalf("volume file not created: %v", err)
	}
	if info.Size() < 1024*1024 {
		t.Errorf("volume file size = %d, want >= %d", info.Size(), 1024*1024)
	}
}

func TestCreateVolume_Idempotent(t *testing.T) {
	p := newTestProvider(t)
	volPath := p.volumePath("idempotent-vol")

	if err := os.WriteFile(volPath, []byte("existing data"), 0600); err != nil {
		t.Fatal(err)
	}

	info, err := p.CreateVolume(ctx, "idempotent-vol", 2048)
	if err != nil {
		t.Fatalf("CreateVolume on existing file should reuse: %v", err)
	}
	if info.VolumeID != "idempotent-vol" {
		t.Errorf("VolumeID = %q, want %q", info.VolumeID, "idempotent-vol")
	}
	if !strings.Contains(info.Path, filepath.Join(p.config.PoolPath, "csi-vol-idempotent-vol.raw")) {
		t.Errorf("unexpected path: %q", info.Path)
	}

	data, err := os.ReadFile(volPath)
	if err != nil {
		t.Fatalf("failed to read volume file: %v", err)
	}
	if string(data) != "existing data" {
		t.Error("existing volume data should be preserved on reuse")
	}
}
