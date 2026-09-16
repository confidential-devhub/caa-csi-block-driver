// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package libvirt

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestProvider(t *testing.T) *LibvirtProvider {
	t.Helper()
	return &LibvirtProvider{config: Config{PoolPath: t.TempDir()}}
}

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

func TestSnapshotPath(t *testing.T) {
	p := &LibvirtProvider{config: Config{PoolPath: "/data/pool"}}
	got := p.snapshotPath("snap-abc")
	want := "/data/pool/csi-snap-snap-abc.raw"
	if got != want {
		t.Errorf("snapshotPath(%q) = %q, want %q", "snap-abc", got, want)
	}
}

func TestCreateSnapshot(t *testing.T) {
	ctx := context.Background()

	t.Run("creates snapshot from volume", func(t *testing.T) {
		p := newTestProvider(t)
		volData := []byte("volume data to snapshot")
		if err := os.WriteFile(p.volumePath("vol-1"), volData, 0600); err != nil {
			t.Fatal(err)
		}

		info, err := p.CreateSnapshot(ctx, "vol-1", "snap-1")
		if err != nil {
			t.Fatalf("CreateSnapshot failed: %v", err)
		}
		if info.SnapshotID != "snap-1" {
			t.Errorf("SnapshotID = %q, want %q", info.SnapshotID, "snap-1")
		}
		if info.SourceVolumeID != "vol-1" {
			t.Errorf("SourceVolumeID = %q, want %q", info.SourceVolumeID, "vol-1")
		}
		if !info.ReadyToUse {
			t.Error("ReadyToUse should be true")
		}

		snapData, err := os.ReadFile(p.snapshotPath("snap-1"))
		if err != nil {
			t.Fatalf("failed to read snapshot file: %v", err)
		}
		if string(snapData) != string(volData) {
			t.Errorf("snapshot data = %q, want %q", string(snapData), string(volData))
		}
	})

	t.Run("writes source volume to meta file", func(t *testing.T) {
		p := newTestProvider(t)
		if err := os.WriteFile(p.volumePath("vol-m"), []byte("data"), 0600); err != nil {
			t.Fatal(err)
		}

		_, err := p.CreateSnapshot(ctx, "vol-m", "snap-m")
		if err != nil {
			t.Fatalf("CreateSnapshot failed: %v", err)
		}

		source := p.readSnapshotSource("snap-m")
		if source != "vol-m" {
			t.Errorf("meta source = %q, want %q", source, "vol-m")
		}
	})

	t.Run("errors on missing source volume", func(t *testing.T) {
		p := newTestProvider(t)
		_, err := p.CreateSnapshot(ctx, "nonexistent", "snap-x")
		if err == nil {
			t.Fatal("expected error for missing source")
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Errorf("error should mention 'not found': %v", err)
		}
	})

	t.Run("idempotent when snapshot exists", func(t *testing.T) {
		p := newTestProvider(t)
		if err := os.WriteFile(p.volumePath("vol-2"), []byte("data"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p.snapshotPath("snap-dup"), []byte("existing snap"), 0600); err != nil {
			t.Fatal(err)
		}

		info, err := p.CreateSnapshot(ctx, "vol-2", "snap-dup")
		if err != nil {
			t.Fatalf("expected idempotent reuse, got error: %v", err)
		}
		if info.SnapshotID != "snap-dup" {
			t.Errorf("SnapshotID = %q, want %q", info.SnapshotID, "snap-dup")
		}

		data, err := os.ReadFile(p.snapshotPath("snap-dup"))
		if err != nil {
			t.Fatalf("failed to read snapshot: %v", err)
		}
		if string(data) != "existing snap" {
			t.Error("existing snapshot should not be overwritten")
		}
	})
}

func TestDeleteSnapshot(t *testing.T) {
	ctx := context.Background()

	t.Run("deletes existing snapshot", func(t *testing.T) {
		p := newTestProvider(t)
		snapPath := p.snapshotPath("snap-1")
		if err := os.WriteFile(snapPath, []byte("snap"), 0600); err != nil {
			t.Fatal(err)
		}

		if err := p.DeleteSnapshot(ctx, "snap-1"); err != nil {
			t.Fatalf("DeleteSnapshot failed: %v", err)
		}
		if _, err := os.Stat(snapPath); !os.IsNotExist(err) {
			t.Error("snapshot file should be gone after delete")
		}
	})

	t.Run("cleans up meta file", func(t *testing.T) {
		p := newTestProvider(t)
		if err := os.WriteFile(p.snapshotPath("snap-m"), []byte("snap"), 0600); err != nil {
			t.Fatal(err)
		}
		p.writeSnapshotSource("snap-m", "vol-x")

		if err := p.DeleteSnapshot(ctx, "snap-m"); err != nil {
			t.Fatalf("DeleteSnapshot failed: %v", err)
		}
		if _, err := os.Stat(p.snapshotMetaPath("snap-m")); !os.IsNotExist(err) {
			t.Error("meta file should be gone after delete")
		}
	})

	t.Run("idempotent on missing snapshot", func(t *testing.T) {
		p := newTestProvider(t)
		if err := p.DeleteSnapshot(ctx, "nonexistent"); err != nil {
			t.Fatalf("DeleteSnapshot on missing snapshot should not error: %v", err)
		}
	})
}

func TestFindSnapshot(t *testing.T) {
	ctx := context.Background()

	t.Run("returns info for existing snapshot", func(t *testing.T) {
		p := newTestProvider(t)
		snapData := []byte("find me")
		if err := os.WriteFile(p.snapshotPath("snap-find"), snapData, 0600); err != nil {
			t.Fatal(err)
		}
		p.writeSnapshotSource("snap-find", "source-vol")

		info, err := p.FindSnapshot(ctx, "snap-find")
		if err != nil {
			t.Fatalf("FindSnapshot failed: %v", err)
		}
		if info == nil {
			t.Fatal("expected non-nil snapshot info")
		}
		if info.SnapshotID != "snap-find" {
			t.Errorf("SnapshotID = %q, want %q", info.SnapshotID, "snap-find")
		}
		if info.SourceVolumeID != "source-vol" {
			t.Errorf("SourceVolumeID = %q, want %q", info.SourceVolumeID, "source-vol")
		}
		if info.SizeBytes != int64(len(snapData)) {
			t.Errorf("SizeBytes = %d, want %d", info.SizeBytes, len(snapData))
		}
	})

	t.Run("returns nil for missing snapshot", func(t *testing.T) {
		p := newTestProvider(t)
		info, err := p.FindSnapshot(ctx, "nonexistent")
		if err != nil {
			t.Fatalf("FindSnapshot should not error for missing: %v", err)
		}
		if info != nil {
			t.Error("expected nil for missing snapshot")
		}
	})
}

func TestListSnapshots(t *testing.T) {
	ctx := context.Background()

	t.Run("empty dir returns empty", func(t *testing.T) {
		p := newTestProvider(t)
		snaps, err := p.ListSnapshots(ctx, "")
		if err != nil {
			t.Fatalf("ListSnapshots failed: %v", err)
		}
		if len(snaps) != 0 {
			t.Errorf("expected 0 snapshots, got %d", len(snaps))
		}
	})

	t.Run("lists snapshots ignoring non-snap files", func(t *testing.T) {
		p := newTestProvider(t)
		if err := os.WriteFile(p.snapshotPath("s1"), []byte("a"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p.snapshotPath("s2"), []byte("bb"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p.volumePath("vol-1"), []byte("vol"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p.config.PoolPath, "random.txt"), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}

		snaps, err := p.ListSnapshots(ctx, "")
		if err != nil {
			t.Fatalf("ListSnapshots failed: %v", err)
		}
		if len(snaps) != 2 {
			t.Fatalf("expected 2 snapshots, got %d", len(snaps))
		}

		ids := map[string]bool{}
		for _, s := range snaps {
			ids[s.SnapshotID] = true
		}
		if !ids["s1"] || !ids["s2"] {
			t.Errorf("expected snapshots s1 and s2, got %v", ids)
		}
	})

	t.Run("filters by source volume ID", func(t *testing.T) {
		p := newTestProvider(t)
		if err := os.WriteFile(p.snapshotPath("s-from-a"), []byte("a"), 0600); err != nil {
			t.Fatal(err)
		}
		p.writeSnapshotSource("s-from-a", "vol-a")

		if err := os.WriteFile(p.snapshotPath("s-from-b"), []byte("b"), 0600); err != nil {
			t.Fatal(err)
		}
		p.writeSnapshotSource("s-from-b", "vol-b")

		snaps, err := p.ListSnapshots(ctx, "vol-a")
		if err != nil {
			t.Fatalf("ListSnapshots failed: %v", err)
		}
		if len(snaps) != 1 {
			t.Fatalf("expected 1 snapshot for vol-a, got %d", len(snaps))
		}
		if snaps[0].SnapshotID != "s-from-a" {
			t.Errorf("expected s-from-a, got %q", snaps[0].SnapshotID)
		}
		if snaps[0].SourceVolumeID != "vol-a" {
			t.Errorf("SourceVolumeID = %q, want %q", snaps[0].SourceVolumeID, "vol-a")
		}
	})
}

func TestListManagedVolumes(t *testing.T) {
	ctx := context.Background()

	t.Run("empty dir returns empty", func(t *testing.T) {
		p := newTestProvider(t)
		vols, err := p.ListManagedVolumes(ctx)
		if err != nil {
			t.Fatalf("ListManagedVolumes failed: %v", err)
		}
		if len(vols) != 0 {
			t.Errorf("expected 0 volumes, got %d", len(vols))
		}
	})

	t.Run("lists volumes ignoring non-volume files", func(t *testing.T) {
		p := newTestProvider(t)
		if err := os.WriteFile(p.volumePath("pvc-aaa"), make([]byte, 2048), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p.volumePath("pvc-bbb"), make([]byte, 4096), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p.snapshotPath("snap-1"), []byte("snap"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p.config.PoolPath, "notes.txt"), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}

		vols, err := p.ListManagedVolumes(ctx)
		if err != nil {
			t.Fatalf("ListManagedVolumes failed: %v", err)
		}
		if len(vols) != 2 {
			t.Fatalf("expected 2 volumes, got %d", len(vols))
		}

		ids := map[string]bool{}
		for _, v := range vols {
			ids[v.VolumeID] = true
			if v.Provider != "libvirt" {
				t.Errorf("Provider = %q, want libvirt", v.Provider)
			}
		}
		if !ids["pvc-aaa"] || !ids["pvc-bbb"] {
			t.Errorf("expected volumes pvc-aaa and pvc-bbb, got %v", ids)
		}
	})

	t.Run("extracts correct size", func(t *testing.T) {
		p := newTestProvider(t)
		if err := os.WriteFile(p.volumePath("sized-vol"), make([]byte, 8192), 0600); err != nil {
			t.Fatal(err)
		}

		vols, err := p.ListManagedVolumes(ctx)
		if err != nil {
			t.Fatalf("ListManagedVolumes failed: %v", err)
		}
		if len(vols) != 1 {
			t.Fatalf("expected 1 volume, got %d", len(vols))
		}
		if vols[0].SizeBytes != 8192 {
			t.Errorf("SizeBytes = %d, want 8192", vols[0].SizeBytes)
		}
	})
}

func TestCreateVolumeFromSnapshot(t *testing.T) {
	ctx := context.Background()

	t.Run("errors on missing snapshot", func(t *testing.T) {
		p := newTestProvider(t)
		_, err := p.CreateVolumeFromSnapshot(ctx, "new-vol", "nonexistent", 1024)
		if err == nil {
			t.Fatal("expected error for missing snapshot")
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Errorf("error should mention 'not found': %v", err)
		}
	})

	t.Run("restores volume from snapshot", func(t *testing.T) {
		p := newTestProvider(t)
		snapData := []byte("snapshot contents here")
		if err := os.WriteFile(p.snapshotPath("snap-1"), snapData, 0600); err != nil {
			t.Fatal(err)
		}

		info, err := p.CreateVolumeFromSnapshot(ctx, "restored-vol", "snap-1", int64(len(snapData)))
		if err != nil {
			t.Fatalf("CreateVolumeFromSnapshot failed: %v", err)
		}
		if info.VolumeID != "restored-vol" {
			t.Errorf("VolumeID = %q, want %q", info.VolumeID, "restored-vol")
		}

		data, err := os.ReadFile(p.volumePath("restored-vol"))
		if err != nil {
			t.Fatalf("failed to read restored volume: %v", err)
		}
		if string(data) != string(snapData) {
			t.Errorf("restored data = %q, want %q", string(data), string(snapData))
		}
	})

	t.Run("reuses existing destination", func(t *testing.T) {
		p := newTestProvider(t)
		if err := os.WriteFile(p.snapshotPath("snap-2"), []byte("snap"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p.volumePath("existing"), []byte("keep this"), 0600); err != nil {
			t.Fatal(err)
		}

		info, err := p.CreateVolumeFromSnapshot(ctx, "existing", "snap-2", 100)
		if err != nil {
			t.Fatalf("expected reuse, got error: %v", err)
		}
		if info.VolumeID != "existing" {
			t.Errorf("VolumeID = %q, want %q", info.VolumeID, "existing")
		}

		data, err := os.ReadFile(p.volumePath("existing"))
		if err != nil {
			t.Fatalf("failed to read volume: %v", err)
		}
		if string(data) != "keep this" {
			t.Error("existing volume should not be overwritten")
		}
	})
}

func TestSanitizeID(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"pvc-abc-123", "pvc-abc-123"},
		{"../../../etc/cron.d/evil", "------etc-cron.d-evil"},
		{"foo/bar", "foo-bar"},
		{"normal-id", "normal-id"},
		{"", ""},
	}
	for _, tt := range tests {
		got := sanitizeID(tt.input)
		if got != tt.want {
			t.Errorf("sanitizeID(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestExpandVolume(t *testing.T) {
	ctx := context.Background()

	t.Run("expands existing volume", func(t *testing.T) {
		p := newTestProvider(t)
		if err := os.WriteFile(p.volumePath("vol-exp"), make([]byte, 1024), 0600); err != nil {
			t.Fatal(err)
		}

		if err := p.ExpandVolume(ctx, "vol-exp", 4096); err != nil {
			t.Fatalf("ExpandVolume failed: %v", err)
		}

		info, err := os.Stat(p.volumePath("vol-exp"))
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() != 4096 {
			t.Errorf("size after expand = %d, want 4096", info.Size())
		}
	})

	t.Run("errors on missing volume", func(t *testing.T) {
		p := newTestProvider(t)
		if err := p.ExpandVolume(ctx, "nonexistent", 4096); err == nil {
			t.Fatal("expected error for missing volume")
		}
	})
}
