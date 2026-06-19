// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	provider "github.com/confidential-devhub/caa-csi-block-driver/pkg/provider"
)

var csLogger = log.New(log.Writer(), "[caa-csi/controller] ", log.LstdFlags|log.Lmsgprefix)

type controllerServer struct {
	csi.UnimplementedControllerServer
	store *volumeStore
}

func newControllerServer() *controllerServer {
	return &controllerServer{
		store: newVolumeStore(),
	}
}

func (cs *controllerServer) CreateVolume(_ context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume name missing")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume capabilities missing")
	}
	if err := validateVolumeCapabilities(req.GetVolumeCapabilities()); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "unsupported volume capability: %v", err)
	}

	params := req.GetParameters()

	if et := params["encrypt-type"]; et != "" {
		if params["kbs-key-id"] == "" {
			return nil, status.Error(codes.InvalidArgument,
				"encrypt-type is set but kbs-key-id is missing: encryption requires a KBS key reference")
		}
	}

	capacity := req.GetCapacityRange().GetRequiredBytes()
	if capacity == 0 {
		capacity = 1073741824 // default 1 GiB
	}

	if rec, err := cs.store.Load(req.GetName()); err == nil {
		if rec.CapacityBytes != 0 && rec.CapacityBytes != capacity {
			return nil, status.Errorf(codes.AlreadyExists,
				"volume %s already exists with different capacity (%d != %d)", req.GetName(), rec.CapacityBytes, capacity)
		}
		csLogger.Printf("CreateVolume: %s already exists, returning existing", req.GetName())
		volumeCtx := map[string]string{"cloudProvider": rec.Provider}
		for k, v := range rec.Params {
			volumeCtx[k] = v
		}
		return &csi.CreateVolumeResponse{
			Volume: &csi.Volume{
				VolumeId:      req.GetName(),
				CapacityBytes: rec.CapacityBytes,
				VolumeContext: volumeCtx,
			},
		}, nil
	}

	p, err := provider.NewBlockVolumeProvider(params)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "failed to create provider: %v", err)
	}

	volInfo, err := p.CreateVolume(req.GetName(), capacity)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "provider.CreateVolume failed: %v", err)
	}

	if err := cs.store.Save(&volumeRecord{
		VolumeID:      req.GetName(),
		Provider:      volInfo.Provider,
		Path:          volInfo.Path,
		CapacityBytes: capacity,
		Params:        params,
	}); err != nil {
		csLogger.Printf("WARNING: failed to persist volume record for %s: %v (volume created in cloud but record may be lost)", req.GetName(), err)
	}
	csLogger.Printf("CreateVolume: %s (provider=%s, path=%s)", req.GetName(), volInfo.Provider, volInfo.Path)

	volumeCtx := map[string]string{
		"cloudProvider": volInfo.Provider,
	}
	for k, v := range params {
		volumeCtx[k] = v
	}
	for k, v := range volInfo.Metadata {
		volumeCtx[k] = v
	}

	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      req.GetName(),
			CapacityBytes: capacity,
			VolumeContext: volumeCtx,
		},
	}, nil
}

func (cs *controllerServer) DeleteVolume(_ context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing")
	}

	rec, err := cs.store.Load(volumeID)
	if err != nil {
		csLogger.Printf("DeleteVolume: volume %s not found in store, skipping", volumeID)
		return &csi.DeleteVolumeResponse{}, nil
	}

	p, err := provider.NewBlockVolumeProvider(rec.Params)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create provider for delete: %v", err)
	}

	if err := p.DeleteVolume(volumeID); err != nil {
		errMsg := err.Error()
		if strings.Contains(errMsg, "attached") || strings.Contains(errMsg, "in use") || strings.Contains(errMsg, "InUse") {
			return nil, status.Errorf(codes.FailedPrecondition, "volume %s is still attached to an instance: %v", volumeID, err)
		}
		return nil, status.Errorf(codes.Internal, "provider.DeleteVolume failed: %v", err)
	}

	cs.store.Delete(volumeID)
	csLogger.Printf("DeleteVolume: %s deleted", volumeID)
	return &csi.DeleteVolumeResponse{}, nil
}

func (cs *controllerServer) ControllerGetCapabilities(_ context.Context, _ *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: []*csi.ControllerServiceCapability{
			{
				Type: &csi.ControllerServiceCapability_Rpc{
					Rpc: &csi.ControllerServiceCapability_RPC{
						Type: csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
					},
				},
			},
		},
	}, nil
}

var supportedAccessModes = map[csi.VolumeCapability_AccessMode_Mode]bool{
	csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER:        true,
	csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY:   true,
	csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER: true,
	csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER:  true,
}

func validateVolumeCapabilities(caps []*csi.VolumeCapability) error {
	for _, cap := range caps {
		if cap.GetBlock() != nil {
			return fmt.Errorf("raw block volumes are not supported")
		}
		if cap.GetAccessMode() == nil {
			return fmt.Errorf("access mode is required")
		}
		mode := cap.GetAccessMode().GetMode()
		if !supportedAccessModes[mode] {
			return fmt.Errorf("access mode %s is not supported (only SINGLE_NODE modes are supported for block volumes)", mode)
		}
	}
	return nil
}

func (cs *controllerServer) ValidateVolumeCapabilities(_ context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing")
	}
	if req.GetVolumeCapabilities() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capabilities missing")
	}

	if !cs.store.Exists(req.GetVolumeId()) {
		return nil, status.Errorf(codes.NotFound, "volume %s not found", req.GetVolumeId())
	}

	if err := validateVolumeCapabilities(req.GetVolumeCapabilities()); err != nil {
		return &csi.ValidateVolumeCapabilitiesResponse{
			Message: err.Error(),
		}, nil
	}

	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: req.GetVolumeCapabilities(),
		},
	}, nil
}
