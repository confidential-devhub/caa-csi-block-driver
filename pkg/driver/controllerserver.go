// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	provider "github.com/confidential-devhub/caa-csi-block-driver/pkg/provider"
)

var csLogger = log.New(log.Writer(), "[caa-csi/controller] ", log.LstdFlags|log.Lmsgprefix)

type controllerServer struct {
	csi.UnimplementedControllerServer
	store *volumeStore
}

func newControllerServer() *controllerServer {
	store := newVolumeStore()

	if params := store.AnyParams(); params != nil {
		if err := store.RecoverFromCloud(params); err != nil {
			csLogger.Printf("WARNING: cloud recovery failed (non-fatal): %v", err)
		}
	} else {
		csLogger.Printf("No existing volumes in store, skipping cloud recovery")
	}

	return &controllerServer{
		store: store,
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
	capacity := req.GetCapacityRange().GetRequiredBytes()
	if capacity == 0 {
		capacity = 1073741824 // default 1 GiB
	}

	if rec, err := cs.store.Load(req.GetName()); err == nil {
		if rec.CapacityBytes != 0 && rec.CapacityBytes != capacity {
			return nil, status.Errorf(codes.AlreadyExists,
				"volume %s already exists with different capacity (%d != %d)", req.GetName(), rec.CapacityBytes, capacity)
		}
		csLogger.Printf("CreateVolume: %s already exists (idempotent), returning existing", req.GetName())
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

	var volInfo *provider.VolumeInfo
	if src := req.GetVolumeContentSource(); src != nil {
		cloner, ok := p.(provider.VolumeCloner)
		if !ok {
			return nil, status.Errorf(codes.Unimplemented, "provider does not support creating volumes from source")
		}
		switch {
		case src.GetSnapshot() != nil:
			snapID := src.GetSnapshot().GetSnapshotId()
			csLogger.Printf("CreateVolume: %s from snapshot %s", req.GetName(), snapID)
			volInfo, err = cloner.CreateVolumeFromSnapshot(req.GetName(), snapID, capacity)
			if err != nil {
				errMsg := strings.ToLower(err.Error())
				if strings.Contains(errMsg, "not found") || strings.Contains(errMsg, "not exist") || strings.Contains(errMsg, "no such") {
					return nil, status.Errorf(codes.NotFound, "source snapshot %s not found: %v", snapID, err)
				}
				return nil, status.Errorf(codes.Internal, "CreateVolumeFromSnapshot failed: %v", err)
			}
		case src.GetVolume() != nil:
			srcVolID := src.GetVolume().GetVolumeId()
			csLogger.Printf("CreateVolume: %s cloned from volume %s", req.GetName(), srcVolID)
			volInfo, err = cloner.CreateVolumeFromVolume(req.GetName(), srcVolID, capacity)
			if err != nil {
				errMsg := strings.ToLower(err.Error())
				if strings.Contains(errMsg, "not found") || strings.Contains(errMsg, "not exist") || strings.Contains(errMsg, "no such") {
					return nil, status.Errorf(codes.NotFound, "source volume %s not found: %v", srcVolID, err)
				}
				return nil, status.Errorf(codes.Internal, "CreateVolumeFromVolume failed: %v", err)
			}
		default:
			return nil, status.Error(codes.InvalidArgument, "unsupported VolumeContentSource type")
		}
	} else {
		volInfo, err = p.CreateVolume(req.GetName(), capacity)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "provider.CreateVolume failed: %v", err)
		}
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

	vol := &csi.Volume{
		VolumeId:      req.GetName(),
		CapacityBytes: capacity,
		VolumeContext: volumeCtx,
	}

	if src := req.GetVolumeContentSource(); src != nil {
		vol.ContentSource = src
	}

	if topoReqs := req.GetAccessibilityRequirements(); topoReqs != nil && len(topoReqs.GetPreferred()) > 0 {
		vol.AccessibleTopology = topoReqs.GetPreferred()
	}

	return &csi.CreateVolumeResponse{Volume: vol}, nil
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

func (cs *controllerServer) ControllerExpandVolume(_ context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing")
	}

	requiredBytes := req.GetCapacityRange().GetRequiredBytes()
	if requiredBytes == 0 {
		return nil, status.Error(codes.InvalidArgument, "Required capacity missing")
	}

	rec, err := cs.store.Load(volumeID)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "volume %s not found: %v", volumeID, err)
	}

	if rec.CapacityBytes >= requiredBytes {
		csLogger.Printf("ControllerExpandVolume: %s already at capacity %d >= %d", volumeID, rec.CapacityBytes, requiredBytes)
		return &csi.ControllerExpandVolumeResponse{
			CapacityBytes:         rec.CapacityBytes,
			NodeExpansionRequired: false,
		}, nil
	}

	p, err := provider.NewBlockVolumeProvider(rec.Params)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create provider for expand: %v", err)
	}

	if expander, ok := p.(provider.VolumeExpander); ok {
		if err := expander.ExpandVolume(volumeID, requiredBytes); err != nil {
			return nil, status.Errorf(codes.Internal, "provider.ExpandVolume failed: %v", err)
		}
	} else {
		return nil, status.Errorf(codes.Unimplemented, "provider %s does not support volume expansion", rec.Provider)
	}

	rec.CapacityBytes = requiredBytes
	if err := cs.store.Save(rec); err != nil {
		csLogger.Printf("WARNING: failed to update volume record for %s after expand: %v", volumeID, err)
	}

	csLogger.Printf("ControllerExpandVolume: %s expanded to %d bytes", volumeID, requiredBytes)
	return &csi.ControllerExpandVolumeResponse{
		CapacityBytes:         requiredBytes,
		NodeExpansionRequired: true,
	}, nil
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
			{
				Type: &csi.ControllerServiceCapability_Rpc{
					Rpc: &csi.ControllerServiceCapability_RPC{
						Type: csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
					},
				},
			},
			{
				Type: &csi.ControllerServiceCapability_Rpc{
					Rpc: &csi.ControllerServiceCapability_RPC{
						Type: csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
					},
				},
			},
			{
				Type: &csi.ControllerServiceCapability_Rpc{
					Rpc: &csi.ControllerServiceCapability_RPC{
						Type: csi.ControllerServiceCapability_RPC_LIST_SNAPSHOTS,
					},
				},
			},
			{
				Type: &csi.ControllerServiceCapability_Rpc{
					Rpc: &csi.ControllerServiceCapability_RPC{
						Type: csi.ControllerServiceCapability_RPC_CLONE_VOLUME,
					},
				},
			},
		},
	}, nil
}

func (cs *controllerServer) CreateSnapshot(_ context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	sourceVolumeID := req.GetSourceVolumeId()
	if sourceVolumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "Source volume ID missing")
	}
	snapshotName := req.GetName()
	if snapshotName == "" {
		return nil, status.Error(codes.InvalidArgument, "Snapshot name missing")
	}

	rec, err := cs.store.Load(sourceVolumeID)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "source volume %s not found: %v", sourceVolumeID, err)
	}

	p, err := provider.NewBlockVolumeProvider(rec.Params)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create provider: %v", err)
	}

	snapshotter, ok := p.(provider.VolumeSnapshotter)
	if !ok {
		return nil, status.Errorf(codes.Unimplemented, "provider %s does not support snapshots", rec.Provider)
	}

	existingSnaps, _ := snapshotter.ListSnapshots(sourceVolumeID)
	for _, s := range existingSnaps {
		if s.SnapshotID == snapshotName {
			csLogger.Printf("CreateSnapshot: %s already exists (idempotent), returning existing", snapshotName)
			return &csi.CreateSnapshotResponse{
				Snapshot: &csi.Snapshot{
					SnapshotId:     s.SnapshotID,
					SourceVolumeId: s.SourceVolumeID,
					SizeBytes:      s.SizeBytes,
					CreationTime:   timestamppb.New(time.Unix(s.CreationTime, 0)),
					ReadyToUse:     s.ReadyToUse,
				},
			}, nil
		}
	}

	snapInfo, err := snapshotter.CreateSnapshot(sourceVolumeID, snapshotName)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "provider.CreateSnapshot failed: %v", err)
	}

	csLogger.Printf("CreateSnapshot: %s from %s", snapshotName, sourceVolumeID)
	return &csi.CreateSnapshotResponse{
		Snapshot: &csi.Snapshot{
			SnapshotId:     snapInfo.SnapshotID,
			SourceVolumeId: sourceVolumeID,
			SizeBytes:      snapInfo.SizeBytes,
			CreationTime:   timestamppb.New(time.Unix(snapInfo.CreationTime, 0)),
			ReadyToUse:     snapInfo.ReadyToUse,
		},
	}, nil
}

func (cs *controllerServer) DeleteSnapshot(_ context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	snapshotID := req.GetSnapshotId()
	if snapshotID == "" {
		return nil, status.Error(codes.InvalidArgument, "Snapshot ID missing")
	}

	params := cs.store.AnyParams()
	if params == nil {
		csLogger.Printf("DeleteSnapshot: no volumes in store, cannot determine provider for snapshot %s", snapshotID)
		return &csi.DeleteSnapshotResponse{}, nil
	}

	p, err := provider.NewBlockVolumeProvider(params)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create provider: %v", err)
	}

	snapshotter, ok := p.(provider.VolumeSnapshotter)
	if !ok {
		return nil, status.Errorf(codes.Unimplemented, "provider does not support snapshots")
	}

	if err := snapshotter.DeleteSnapshot(snapshotID); err != nil {
		return nil, status.Errorf(codes.Internal, "provider.DeleteSnapshot failed: %v", err)
	}

	csLogger.Printf("DeleteSnapshot: %s deleted", snapshotID)
	return &csi.DeleteSnapshotResponse{}, nil
}

func (cs *controllerServer) ListSnapshots(_ context.Context, req *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	sourceVolumeID := req.GetSourceVolumeId()

	var params map[string]string
	if sourceVolumeID != "" {
		rec, err := cs.store.Load(sourceVolumeID)
		if err != nil {
			return &csi.ListSnapshotsResponse{}, nil
		}
		params = rec.Params
	} else {
		params = cs.store.AnyParams()
	}

	if params == nil {
		return &csi.ListSnapshotsResponse{}, nil
	}

	p, err := provider.NewBlockVolumeProvider(params)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create provider: %v", err)
	}

	snapshotter, ok := p.(provider.VolumeSnapshotter)
	if !ok {
		return &csi.ListSnapshotsResponse{}, nil
	}

	snapInfos, err := snapshotter.ListSnapshots(sourceVolumeID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "provider.ListSnapshots failed: %v", err)
	}

	maxEntries := int(req.GetMaxEntries())
	startIdx := 0
	if token := req.GetStartingToken(); token != "" {
		if idx, err := strconv.Atoi(token); err == nil {
			startIdx = idx
		}
	}

	var entries []*csi.ListSnapshotsResponse_Entry
	for i := startIdx; i < len(snapInfos); i++ {
		if maxEntries > 0 && len(entries) >= maxEntries {
			return &csi.ListSnapshotsResponse{
				Entries:   entries,
				NextToken: strconv.Itoa(i),
			}, nil
		}
		s := snapInfos[i]
		entries = append(entries, &csi.ListSnapshotsResponse_Entry{
			Snapshot: &csi.Snapshot{
				SnapshotId:     s.SnapshotID,
				SourceVolumeId: s.SourceVolumeID,
				SizeBytes:      s.SizeBytes,
				CreationTime:   timestamppb.New(time.Unix(s.CreationTime, 0)),
				ReadyToUse:     s.ReadyToUse,
			},
		})
	}

	return &csi.ListSnapshotsResponse{Entries: entries}, nil
}

// supportedAccessModes lists the access modes this CSI driver supports.
// Block volumes can only attach to one VM, so only SINGLE_NODE modes work.
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
		if cap.GetAccessMode() != nil {
			mode := cap.GetAccessMode().GetMode()
			if !supportedAccessModes[mode] {
				return fmt.Errorf("access mode %s is not supported (only SINGLE_NODE modes are supported for block volumes)", mode)
			}
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
