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

func TestNodePublishVolume(t *testing.T) {
	publish := func(t *testing.T, mount *csi.VolumeCapability_MountVolume) mountInfoJSON {
		t.Helper()
		rootDir := t.TempDir()
		t.Setenv("KATA_DIRECT_VOLUME_ROOT_PATH", rootDir)
		targetPath := filepath.Join(t.TempDir(), "pods", "uid", "volumes", "kubernetes.io~csi", "pvc", "mount")

		ns := &nodeServer{devices: map[string]string{"vol-1": "vol-0123456789abcdef0"}}
		_, err := ns.NodePublishVolume(t.Context(), &csi.NodePublishVolumeRequest{
			VolumeId:   "vol-1",
			TargetPath: targetPath,
			VolumeCapability: &csi.VolumeCapability{
				AccessType: &csi.VolumeCapability_Mount{Mount: mount},
			},
		})
		if err != nil {
			t.Fatalf("NodePublishVolume() error = %v", err)
		}

		data, err := os.ReadFile(filepath.Join(rootDir, b64.URLEncoding.EncodeToString([]byte(targetPath)), mountInfoFileName))
		if err != nil {
			t.Fatal(err)
		}
		var info mountInfoJSON
		if err := json.Unmarshal(data, &info); err != nil {
			t.Fatal(err)
		}
		return info
	}

	t.Run("records the pod fsGroup for the pod VM", func(t *testing.T) {
		info := publish(t, &csi.VolumeCapability_MountVolume{FsType: "ext4", VolumeMountGroup: "2000"})
		if got := info.Metadata["fsGroup"]; got != "2000" {
			t.Fatalf("metadata fsGroup = %q, want %q", got, "2000")
		}
	})

	t.Run("omits fsGroup when the pod sets none", func(t *testing.T) {
		info := publish(t, &csi.VolumeCapability_MountVolume{FsType: "ext4"})
		if got, ok := info.Metadata["fsGroup"]; ok {
			t.Fatalf("metadata fsGroup = %q, want it unset", got)
		}
	})
}

func TestNodeGetCapabilities(t *testing.T) {
	resp, err := (&nodeServer{}).NodeGetCapabilities(t.Context(), &csi.NodeGetCapabilitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range resp.GetCapabilities() {
		if c.GetRpc().GetType() == csi.NodeServiceCapability_RPC_VOLUME_MOUNT_GROUP {
			return
		}
	}
	t.Fatal("VOLUME_MOUNT_GROUP not advertised, so kubelet will not pass the pod fsGroup")
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
