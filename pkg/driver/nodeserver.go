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
	volumeStatsTimeout              = 10 * time.Second
)

func getKataDirectVolumeRootPath() string {
	if p := os.Getenv("KATA_DIRECT_VOLUME_ROOT_PATH"); p != "" {
		return p
	}
	return defaultKataDirectVolumeRootPath
}

func getKataRuntimePath() string {
	if p := os.Getenv("KATA_RUNTIME_PATH"); p != "" {
		return p
	}
	return "kata-runtime"
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
	return &nodeServer{
		nodeID:  nodeID,
		devices: make(map[string]string),
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
// It queries the guest VM via kata-runtime (where the filesystem is actually
// mounted). Host-side statfs is only used when CSI_ALLOW_HOST_STATS_FALLBACK
// is set, because the volume is not mounted on the host and statfs would report
// metrics for the host partition rather than the actual volume.
func (ns *nodeServer) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	volumeID := req.GetVolumeId()
	volumePath := req.GetVolumePath()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing")
	}
	if volumePath == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume path missing")
	}

	if _, err := os.Stat(volumePath); err != nil {
		if os.IsNotExist(err) {
			return nil, status.Errorf(codes.NotFound, "volume path %s does not exist", volumePath)
		}
		return nil, status.Errorf(codes.Internal, "failed to stat volume path %s: %v", volumePath, err)
	}

	if resp, err := getVolumeStatsViaRuntime(ctx, volumePath); err == nil {
		nsLogger.Printf("NodeGetVolumeStats: retrieved guest-side stats for volume %s via kata-runtime", volumeID)
		return resp, nil
	} else {
		nsLogger.Printf("NodeGetVolumeStats: kata-runtime stats unavailable for %s: %v", volumeID, err)
	}

	if os.Getenv("CSI_ALLOW_HOST_STATS_FALLBACK") != "" {
		nsLogger.Printf("NodeGetVolumeStats: using host-side statfs for %s (CSI_ALLOW_HOST_STATS_FALLBACK set)", volumeID)
		return getVolumeStatsLocal(volumePath)
	}

	return nil, status.Errorf(codes.Unavailable,
		"volume stats unavailable for %s: kata-runtime is required to query guest-side filesystem metrics", volumeID)
}

func getVolumeStatsViaRuntime(ctx context.Context, volumePath string) (*csi.NodeGetVolumeStatsResponse, error) {
	runtimePath := getKataRuntimePath()
	resolvedPath, err := exec.LookPath(runtimePath)
	if err != nil {
		return nil, fmt.Errorf("kata-runtime not found at %q: %w", runtimePath, err)
	}

	statsCtx, cancel := context.WithTimeout(ctx, volumeStatsTimeout)
	defer cancel()

	out, err := exec.CommandContext(statsCtx, resolvedPath, "direct-volume", "stats", "--volume-path", volumePath).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("kata-runtime direct-volume stats: %s: %w", strings.TrimSpace(string(out)), err)
	}

	return parseKataVolumeStats(out)
}

// parseKataVolumeStats decodes kata-runtime direct-volume stats output.
// kata-runtime prints a CSI VolumeStatsResponse JSON blob:
// {"usage":[{"available":…,"total":…,"used":…,"unit":1}, …]}
func parseKataVolumeStats(data []byte) (*csi.NodeGetVolumeStatsResponse, error) {
	var resp csi.NodeGetVolumeStatsResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parsing kata-runtime stats output: %w", err)
	}
	if len(resp.Usage) == 0 {
		return nil, fmt.Errorf("kata-runtime stats response contained no usage entries")
	}
	hasKnownUnit := false
	for _, u := range resp.Usage {
		if u == nil {
			continue
		}
		if u.Unit == csi.VolumeUsage_BYTES || u.Unit == csi.VolumeUsage_INODES {
			hasKnownUnit = true
			break
		}
	}
	if !hasKnownUnit {
		return nil, fmt.Errorf("kata-runtime stats response contained no BYTES/INODES usage entries")
	}
	return &resp, nil
}

func getVolumeStatsLocal(volumePath string) (*csi.NodeGetVolumeStatsResponse, error) {
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
	usedBytes := (int64(statfs.Blocks) - int64(statfs.Bfree)) * blockSize

	totalInodes := int64(statfs.Files)
	// Linux statfs does not expose Favail; on ext4/xfs Ffree == Favail (no inode reservation).
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
