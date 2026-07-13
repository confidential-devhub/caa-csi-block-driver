// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"context"
	b64 "encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	provider "github.com/confidential-devhub/caa-csi-block-driver/pkg/provider"
)

var nsLogger = log.New(log.Writer(), "[caa-csi/node] ", log.LstdFlags|log.Lmsgprefix)

const (
	defaultKataDirectVolumeRootPath = "/run/kata-containers/shared/direct-volumes"
	mountInfoFileName               = "mountInfo.json"
)

func getKataDirectVolumeRootPath() string {
	if p := os.Getenv("KATA_DIRECT_VOLUME_ROOT_PATH"); p != "" {
		return p
	}
	return defaultKataDirectVolumeRootPath
}

type mountInfoJSON struct {
	VolumeType string            `json:"volume-type"`
	Device     string            `json:"device"`
	FsType     string            `json:"fstype"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	Options    []string          `json:"options,omitempty"`
}

type nodeServer struct {
	csi.UnimplementedNodeServer
	nodeID  string
	mu      sync.Mutex
	devices map[string]string // volumeID → device path or cloud volume ID
}

func newNodeServer(nodeID string) *nodeServer {
	ns := &nodeServer{
		nodeID:  nodeID,
		devices: make(map[string]string),
	}
	ns.cleanStaleMountInfoFiles()
	return ns
}

func (ns *nodeServer) cleanStaleMountInfoFiles() {
	root := getKataDirectVolumeRootPath()
	entries, err := os.ReadDir(root)
	if err != nil {
		nsLogger.Printf("WARNING: cannot read direct-volumes root %s: %v, skipping cleanup", root, err)
		return
	}

	store := newVolumeStore()
	if !store.IsAccessible() {
		nsLogger.Printf("WARNING: volume store directory is not accessible, skipping cleanup to avoid deleting valid entries")
		return
	}

	removed := 0

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		infoPath := filepath.Join(root, e.Name(), mountInfoFileName)
		data, err := os.ReadFile(infoPath)
		if err != nil {
			continue
		}

		var info mountInfoJSON
		if err := json.Unmarshal(data, &info); err != nil {
			nsLogger.Printf("Removing corrupt mountInfo dir: %s", e.Name())
			os.RemoveAll(filepath.Join(root, e.Name()))
			removed++
			continue
		}

		volID := info.Metadata["cloud-volume-id"]
		if volID == "" {
			continue
		}

		exists, err := store.Exists(volID)
		if err != nil {
			nsLogger.Printf("WARNING: cannot check volume store for %s: %v, skipping", volID, err)
			continue
		}
		if !exists {
			nsLogger.Printf("Removing stale mountInfo for volume %s (no matching volume record)", volID)
			os.RemoveAll(filepath.Join(root, e.Name()))
			removed++
		}
	}

	if removed > 0 {
		nsLogger.Printf("Cleaned up %d stale mountInfo entries on startup", removed)
	}
}

func (ns *nodeServer) NodeStageVolume(_ context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing")
	}
	if req.GetStagingTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "Staging target path missing")
	}
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capability missing")
	}

	params := req.GetVolumeContext()

	p, err := provider.NewBlockVolumeProvider(params)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create provider: %v", err)
	}

	var sizeBytes int64 = 1073741824
	if capacityStr := params["capacity_in_bytes"]; capacityStr != "" {
		if parsed, err := strconv.ParseInt(capacityStr, 10, 64); err == nil && parsed > 0 {
			sizeBytes = parsed
		} else {
			nsLogger.Printf("WARNING: invalid capacity_in_bytes %q, using default 1GiB", capacityStr)
		}
	}

	volInfo, err := p.CreateVolume(volumeID, sizeBytes)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "provider.CreateVolume failed: %v", err)
	}

	ns.mu.Lock()
	ns.devices[volumeID] = volInfo.Path
	ns.mu.Unlock()
	nsLogger.Printf("NodeStageVolume: %s staged (provider=%s, path=%s)", volumeID, volInfo.Provider, volInfo.Path)

	return &csi.NodeStageVolumeResponse{}, nil
}

func (ns *nodeServer) NodePublishVolume(_ context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	targetPath := req.GetTargetPath()

	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing")
	}
	if targetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "Target path missing")
	}
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capability missing")
	}

	ns.mu.Lock()
	devicePath := ns.devices[volumeID]
	ns.mu.Unlock()

	if devicePath == "" {
		return nil, status.Errorf(codes.FailedPrecondition, "volume %s not staged (no device path)", volumeID)
	}

	attrib := req.GetVolumeContext()
	if attrib == nil {
		attrib = make(map[string]string)
	}

	attrib["cloud-volume-id"] = volumeID
	if attrib["cloud-volume-path"] == "" {
		attrib["cloud-volume-path"] = devicePath
	}

	info := mountInfoJSON{
		VolumeType: "directvol",
		Device:     devicePath,
		FsType:     "ext4",
		Metadata:   attrib,
	}

	data, err := json.Marshal(info)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to marshal mountInfo: %v", err)
	}

	volumeDir := filepath.Join(getKataDirectVolumeRootPath(), b64.URLEncoding.EncodeToString([]byte(targetPath)))
	if err := os.MkdirAll(volumeDir, 0700); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create kata direct volume dir %s: %v", volumeDir, err)
	}
	if err := os.WriteFile(filepath.Join(volumeDir, mountInfoFileName), data, 0600); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to write mountInfo.json: %v", err)
	}

	if err := os.MkdirAll(targetPath, 0750); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create target path %s: %v", targetPath, err)
	}

	nsLogger.Printf("NodePublishVolume: %s published at %s (device=%s, provider=%s)",
		volumeID, targetPath, devicePath, attrib["cloud-provider"])

	return &csi.NodePublishVolumeResponse{}, nil
}

func (ns *nodeServer) NodeUnpublishVolume(_ context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	targetPath := req.GetTargetPath()
	if targetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "Target path missing")
	}

	volumeDir := filepath.Join(getKataDirectVolumeRootPath(), b64.URLEncoding.EncodeToString([]byte(targetPath)))
	if err := os.RemoveAll(volumeDir); err != nil {
		nsLogger.Printf("WARNING: failed to remove kata direct volume dir %s: %v", volumeDir, err)
	}

	if err := os.RemoveAll(targetPath); err != nil {
		nsLogger.Printf("WARNING: failed to remove target path %s: %v", targetPath, err)
	}

	nsLogger.Printf("NodeUnpublishVolume: %s unpublished", req.GetVolumeId())
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (ns *nodeServer) NodeUnstageVolume(_ context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing")
	}
	if req.GetStagingTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "Staging target path missing")
	}

	ns.mu.Lock()
	delete(ns.devices, volumeID)
	ns.mu.Unlock()

	nsLogger.Printf("NodeUnstageVolume: %s unstaged", volumeID)
	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (ns *nodeServer) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	volumePath := req.GetVolumePath()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing")
	}
	if volumePath == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume path missing")
	}

	requiredBytes := req.GetCapacityRange().GetRequiredBytes()
	if requiredBytes == 0 {
		return nil, status.Error(codes.InvalidArgument, "Required capacity missing")
	}

	ns.mu.Lock()
	devicePath := ns.devices[volumeID]
	ns.mu.Unlock()

	if devicePath == "" || !strings.HasPrefix(devicePath, "/") {
		nsLogger.Printf("NodeExpandVolume: device path %q for volume %s is not a local path, filesystem resize will happen inside PodVM", devicePath, volumeID)
		return &csi.NodeExpandVolumeResponse{CapacityBytes: requiredBytes}, nil
	}

	if err := resizeFilesystem(ctx, devicePath, volumePath); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to resize filesystem on %s: %v", devicePath, err)
	}

	nsLogger.Printf("NodeExpandVolume: %s resized at %s", volumeID, volumePath)
	return &csi.NodeExpandVolumeResponse{CapacityBytes: requiredBytes}, nil
}

const resizeTimeout = 30 * time.Second

func resizeFilesystem(ctx context.Context, devicePath, mountPath string) error {
	ctx, cancel := context.WithTimeout(ctx, resizeTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "blkid", "-o", "value", "-s", "TYPE", devicePath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("detecting filesystem type on %s: %s: %w", devicePath, strings.TrimSpace(string(out)), err)
	}
	fsType := strings.TrimSpace(string(out))

	switch fsType {
	case "ext4", "ext3", "ext2":
		if out, err := exec.CommandContext(ctx, "resize2fs", devicePath).CombinedOutput(); err != nil {
			return fmt.Errorf("resize2fs %s: %s: %w", devicePath, strings.TrimSpace(string(out)), err)
		}
	case "xfs":
		if out, err := exec.CommandContext(ctx, "xfs_growfs", mountPath).CombinedOutput(); err != nil {
			return fmt.Errorf("xfs_growfs %s: %s: %w", mountPath, strings.TrimSpace(string(out)), err)
		}
	default:
		return fmt.Errorf("unsupported filesystem %q for online resize", fsType)
	}
	return nil
}

// NodeGetVolumeStats returns filesystem usage statistics for the given volume.
func (ns *nodeServer) NodeGetVolumeStats(_ context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	volumeID := req.GetVolumeId()
	volumePath := req.GetVolumePath()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing")
	}
	if volumePath == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume path missing")
	}

	if _, err := os.Stat(volumePath); os.IsNotExist(err) {
		return nil, status.Errorf(codes.NotFound, "volume path %s does not exist", volumePath)
	}

	var statfs syscall.Statfs_t
	if err := syscall.Statfs(volumePath, &statfs); err != nil {
		return nil, status.Errorf(codes.Internal, "statfs on %s failed: %v", volumePath, err)
	}

	blockSize := int64(statfs.Frsize)
	if blockSize == 0 {
		blockSize = int64(statfs.Bsize)
	}
	totalBytes := int64(statfs.Blocks) * blockSize
	availBytes := int64(statfs.Bavail) * blockSize
	usedBytes := totalBytes - availBytes

	totalInodes := int64(statfs.Files)
	freeInodes := int64(statfs.Ffree)
	usedInodes := totalInodes - freeInodes

	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{
			{
				Available: availBytes,
				Total:     totalBytes,
				Used:      usedBytes,
				Unit:      csi.VolumeUsage_BYTES,
			},
			{
				Available: freeInodes,
				Total:     totalInodes,
				Used:      usedInodes,
				Unit:      csi.VolumeUsage_INODES,
			},
		},
	}, nil
}

func (ns *nodeServer) NodeGetCapabilities(_ context.Context, _ *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
					},
				},
			},
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
					},
				},
			},
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
					},
				},
			},
		},
	}, nil
}

func (ns *nodeServer) NodeGetInfo(_ context.Context, _ *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	resp := &csi.NodeGetInfoResponse{
		NodeId: ns.nodeID,
	}

	region := os.Getenv("CSI_TOPOLOGY_REGION")
	zone := os.Getenv("CSI_TOPOLOGY_ZONE")
	if region != "" || zone != "" {
		segments := make(map[string]string)
		if region != "" {
			segments["topology.caa-csi.io/region"] = region
		}
		if zone != "" {
			segments["topology.caa-csi.io/zone"] = zone
		}
		resp.AccessibleTopology = &csi.Topology{Segments: segments}
		nsLogger.Printf("NodeGetInfo: advertising topology segments: %v", segments)
	}

	return resp, nil
}
