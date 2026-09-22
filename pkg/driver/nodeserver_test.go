// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	b64 "encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

func TestParseKataVolumeStats(t *testing.T) {
	t.Parallel()

	valid := []byte(`{
		"usage": [
			{"available": 100, "total": 200, "used": 100, "unit": 1},
			{"available": 50, "total": 100, "used": 50, "unit": 2}
		],
		"volume_condition": {"abnormal": false, "message": "OK"}
	}`)

	resp, err := parseKataVolumeStats(valid)
	if err != nil {
		t.Fatalf("expected valid kata stats JSON to parse, got: %v", err)
	}
	if len(resp.Usage) != 2 {
		t.Fatalf("expected 2 usage entries, got %d", len(resp.Usage))
	}
	if resp.Usage[0].Unit != csi.VolumeUsage_BYTES || resp.Usage[0].Total != 200 || resp.Usage[0].Used != 100 {
		t.Fatalf("unexpected bytes usage: %+v", resp.Usage[0])
	}
	if resp.Usage[1].Unit != csi.VolumeUsage_INODES || resp.Usage[1].Total != 100 || resp.Usage[1].Available != 50 {
		t.Fatalf("unexpected inode usage: %+v", resp.Usage[1])
	}

	// Old incorrect flat schema must not silently succeed with zeros.
	flat := []byte(`{"usedBytes":10,"totalBytes":20,"availBytes":10,"usedInodes":1,"totalInodes":2,"freeInodes":1}`)
	if _, err := parseKataVolumeStats(flat); err == nil {
		t.Fatal("expected flat usedBytes schema to be rejected")
	}

	if _, err := parseKataVolumeStats([]byte(`{"usage":[]}`)); err == nil {
		t.Fatal("expected empty usage array to be rejected")
	}

	if _, err := parseKataVolumeStats([]byte(`not-json`)); err == nil {
		t.Fatal("expected invalid JSON to be rejected")
	}
}

func TestRequestedFsType(t *testing.T) {
	tests := []struct {
		name       string
		capability *csi.VolumeCapability
		context    map[string]string
		want       string
	}{
		{
			name: "mount capability takes precedence",
			capability: &csi.VolumeCapability{
				AccessType: &csi.VolumeCapability_Mount{
					Mount: &csi.VolumeCapability_MountVolume{FsType: "xfs"},
				},
			},
			context: map[string]string{"csi.storage.k8s.io/fstype": "ext4"},
			want:    "xfs",
		},
		{
			name:    "volume context fallback",
			context: map[string]string{"csi.storage.k8s.io/fstype": "xfs"},
			want:    "xfs",
		},
		{
			name: "default",
			want: "ext4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := requestedFsType(tt.capability, tt.context); got != tt.want {
				t.Fatalf("requestedFsType() = %q, want %q", got, tt.want)
			}
		})
	}
}

// writeMountInfoDir creates a base64-encoded dir under rootDir with a
// mountInfo.json inside it. Returns the dir path.
func writeMountInfoDir(t *testing.T, rootDir, targetPath string, info *mountInfoJSON) string {
	t.Helper()
	dirName := b64.URLEncoding.EncodeToString([]byte(targetPath))
	dirPath := filepath.Join(rootDir, dirName)
	if err := os.MkdirAll(dirPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if info != nil {
		data, err := json.Marshal(info)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dirPath, mountInfoFileName), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dirPath
}

func TestCleanStaleMountInfoDirs_TargetExists(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	targetPath := t.TempDir()

	dirPath := writeMountInfoDir(t, rootDir, targetPath, &mountInfoJSON{
		VolumeType: "directvol",
		Device:     "/dev/sda",
		FsType:     "ext4",
	})

	cleanStaleMountInfoDirs(rootDir)

	if _, err := os.Stat(dirPath); err != nil {
		t.Fatal("dir should NOT be removed when target path exists")
	}
}

func TestCleanStaleMountInfoDirs_TargetGone(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	targetPath := filepath.Join(t.TempDir(), "gone")

	dirPath := writeMountInfoDir(t, rootDir, targetPath, &mountInfoJSON{
		VolumeType: "directvol",
		Device:     "/dev/sda",
		FsType:     "ext4",
	})

	cleanStaleMountInfoDirs(rootDir)

	if _, err := os.Stat(dirPath); !os.IsNotExist(err) {
		t.Fatal("dir should be removed when target path is gone")
	}
}

func TestCleanStaleMountInfoDirs_CorruptJSON(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	targetPath := "/tmp/corrupt-" + t.Name()
	dirName := b64.URLEncoding.EncodeToString([]byte(targetPath))
	dirPath := filepath.Join(rootDir, dirName)
	if err := os.MkdirAll(dirPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirPath, mountInfoFileName), []byte("not valid json{{{"), 0o644); err != nil {
		t.Fatal(err)
	}

	cleanStaleMountInfoDirs(rootDir)

	if _, err := os.Stat(dirPath); !os.IsNotExist(err) {
		t.Fatal("dir with corrupt mountInfo.json should be removed")
	}
}

func TestCleanStaleMountInfoDirs_NoMountInfoFile(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	targetPath := "/tmp/no-file-" + t.Name()

	dirPath := writeMountInfoDir(t, rootDir, targetPath, nil)

	cleanStaleMountInfoDirs(rootDir)

	if _, err := os.Stat(dirPath); err != nil {
		t.Fatal("dir without mountInfo.json should be left alone")
	}
}

func TestCleanStaleMountInfoDirs_EmptyRoot(t *testing.T) {
	t.Parallel()

	cleanStaleMountInfoDirs(t.TempDir())
}

func TestCleanStaleMountInfoDirs_NonexistentRoot(t *testing.T) {
	t.Parallel()

	cleanStaleMountInfoDirs("/nonexistent/path/" + t.Name())
}
