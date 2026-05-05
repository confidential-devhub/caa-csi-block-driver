// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"context"
	"log"

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

	params := req.GetParameters()
	capacity := req.GetCapacityRange().GetRequiredBytes()
	if capacity == 0 {
		capacity = 1073741824 // default 1 GiB
	}

	if rec, err := cs.store.Load(req.GetName()); err == nil {
		if rec.CapacityBytes != 0 && rec.CapacityBytes != capacity {
			return nil, status.Errorf(codes.AlreadyExists,
				"volume %s already exists with different capacity (%d != %d)", req.GetName(), rec.CapacityBytes, capacity)
		}
	}

	p, err := provider.NewBlockVolumeProvider(params)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "failed to create provider: %v", err)
	}

	volInfo, err := p.CreateVolume(req.GetName(), capacity)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "provider.CreateVolume failed: %v", err)
	}

	cs.store.Save(&volumeRecord{
		VolumeID:      req.GetName(),
		Provider:      volInfo.Provider,
		Path:          volInfo.Path,
		CapacityBytes: capacity,
		Params:        params,
	})
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

	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: req.GetVolumeCapabilities(),
		},
	}, nil
}
