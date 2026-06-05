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

func (cs *controllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
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

	var volInfo *provider.VolumeInfo
	if src := req.GetVolumeContentSource(); src != nil {
		switch {
		case src.GetSnapshot() != nil:
			snapID := src.GetSnapshot().GetSnapshotId()
			snapshotter, ok := p.(provider.VolumeSnapshotter)
			if !ok {
				return nil, status.Errorf(codes.NotFound, "snapshot %s not found", snapID)
			}
			snap, findErr := snapshotter.FindSnapshot(ctx, snapID)
			if findErr != nil {
				return nil, status.Errorf(codes.Internal, "failed to verify snapshot %s: %v", snapID, findErr)
			}
			if snap == nil {
				return nil, status.Errorf(codes.NotFound, "snapshot %s not found", snapID)
			}
			cloner, ok := p.(provider.VolumeCloner)
			if !ok {
				return nil, status.Errorf(codes.Unimplemented, "provider does not support creating volumes from snapshot")
			}
			csLogger.Printf("CreateVolume: %s from snapshot %s", req.GetName(), snapID)
			volInfo, err = cloner.CreateVolumeFromSnapshot(req.GetName(), snapID, capacity)
		case src.GetVolume() != nil:
			srcVolID := src.GetVolume().GetVolumeId()
			exists, existsErr := p.VolumeExists(srcVolID)
			if existsErr != nil {
				return nil, status.Errorf(codes.Internal, "failed to verify source volume %s: %v", srcVolID, existsErr)
			}
			if !exists {
				return nil, status.Errorf(codes.NotFound, "source volume %s not found", srcVolID)
			}
			cloner, ok := p.(provider.VolumeCloner)
			if !ok {
				return nil, status.Errorf(codes.Unimplemented, "provider does not support volume cloning")
			}
			csLogger.Printf("CreateVolume: %s cloned from volume %s", req.GetName(), srcVolID)
			volInfo, err = cloner.CreateVolumeFromVolume(req.GetName(), srcVolID, capacity)
		default:
			return nil, status.Error(codes.InvalidArgument, "unsupported VolumeContentSource type")
		}
	} else {
		volInfo, err = p.CreateVolume(req.GetName(), capacity)
	}
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
	cs.store.WriteManifest()
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
	cs.store.WriteManifest()
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
			NodeExpansionRequired: true,
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
		return nil, status.Errorf(codes.Internal, "volume %s expanded in cloud but failed to persist record: %v", volumeID, err)
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

func (cs *controllerServer) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
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

	// Targeted lookup by name to enforce uniqueness per CSI spec without listing all snapshots.
	existing, err := snapshotter.FindSnapshot(ctx, snapshotName)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to check existing snapshot %s: %v", snapshotName, err)
	}
	if existing != nil {
		if existing.SourceVolumeID != sourceVolumeID {
			return nil, status.Errorf(codes.AlreadyExists,
				"snapshot %s already exists for different source volume %s", snapshotName, existing.SourceVolumeID)
		}
		csLogger.Printf("CreateSnapshot: %s already exists, returning existing", snapshotName)
		return &csi.CreateSnapshotResponse{
			Snapshot: &csi.Snapshot{
				SnapshotId:     existing.SnapshotID,
				SourceVolumeId: existing.SourceVolumeID,
				SizeBytes:      existing.SizeBytes,
				CreationTime:   timestamppb.New(time.Unix(existing.CreationTime, 0)),
				ReadyToUse:     existing.ReadyToUse,
			},
		}, nil
	}

	snapInfo, err := snapshotter.CreateSnapshot(ctx, sourceVolumeID, snapshotName)
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

func (cs *controllerServer) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	snapshotID := req.GetSnapshotId()
	if snapshotID == "" {
		return nil, status.Error(codes.InvalidArgument, "Snapshot ID missing")
	}

	// Use secrets from the request if available (preferred for deterministic routing),
	// otherwise fall back to params from any volume in the store.
	params := req.GetSecrets()
	if len(params) == 0 || params["cloudProvider"] == "" {
		params = cs.store.AnyParams()
	}
	if params == nil {
		csLogger.Printf("DeleteSnapshot: no volumes in store and no secrets provided, cannot determine provider for snapshot %s", snapshotID)
		return nil, status.Errorf(codes.FailedPrecondition,
			"cannot determine provider for snapshot %s: no volumes in store and no secrets provided", snapshotID)
	}

	p, err := provider.NewBlockVolumeProvider(params)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create provider: %v", err)
	}

	snapshotter, ok := p.(provider.VolumeSnapshotter)
	if !ok {
		return nil, status.Errorf(codes.Unimplemented, "provider does not support snapshots")
	}

	if err := snapshotter.DeleteSnapshot(ctx, snapshotID); err != nil {
		return nil, status.Errorf(codes.Internal, "provider.DeleteSnapshot failed: %v", err)
	}

	csLogger.Printf("DeleteSnapshot: %s deleted", snapshotID)
	return &csi.DeleteSnapshotResponse{}, nil
}

func (cs *controllerServer) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	sourceVolumeID := req.GetSourceVolumeId()
	snapshotID := req.GetSnapshotId()

	var params map[string]string
	if sourceVolumeID != "" {
		rec, err := cs.store.Load(sourceVolumeID)
		if err != nil {
			return nil, status.Errorf(codes.NotFound, "source volume %s not found: %v", sourceVolumeID, err)
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

	// If a specific snapshot ID is requested, do a targeted lookup.
	if snapshotID != "" {
		snap, err := snapshotter.FindSnapshot(ctx, snapshotID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "provider.FindSnapshot failed: %v", err)
		}
		if snap == nil {
			return &csi.ListSnapshotsResponse{}, nil
		}
		if sourceVolumeID != "" && snap.SourceVolumeID != sourceVolumeID {
			return &csi.ListSnapshotsResponse{}, nil
		}
		return &csi.ListSnapshotsResponse{
			Entries: []*csi.ListSnapshotsResponse_Entry{{
				Snapshot: &csi.Snapshot{
					SnapshotId:     snap.SnapshotID,
					SourceVolumeId: snap.SourceVolumeID,
					SizeBytes:      snap.SizeBytes,
					CreationTime:   timestamppb.New(time.Unix(snap.CreationTime, 0)),
					ReadyToUse:     snap.ReadyToUse,
				},
			}},
		}, nil
	}

	snapInfos, err := snapshotter.ListSnapshots(ctx, sourceVolumeID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "provider.ListSnapshots failed: %v", err)
	}

	maxEntries := int(req.GetMaxEntries())
	startIdx := 0
	if token := req.GetStartingToken(); token != "" {
		idx, err := strconv.Atoi(token)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid starting_token %q: must be a numeric index", token)
		}
		startIdx = idx
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
