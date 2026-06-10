// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"context"
	b64 "encoding/base64"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"sync"

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
		},
	}, nil
}

func (ns *nodeServer) NodeGetInfo(_ context.Context, _ *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{
		NodeId: ns.nodeID,
	}, nil
}
