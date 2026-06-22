// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package provider

// VolumeInfo holds provider-agnostic metadata about a block volume.
type VolumeInfo struct {
	VolumeID  string
	Path      string            // File path (Libvirt) or cloud volume ID (AWS EBS)
	SizeBytes int64
	Provider  string            // "libvirt", "aws"
	Metadata  map[string]string // Provider-specific data passed via mountInfo.json
}

// BlockVolumeProvider is the contract every cloud provider must implement.
// All mutating methods must be idempotent: repeated calls with the same
// arguments should produce the same result without side effects.
type BlockVolumeProvider interface {
	// CreateVolume provisions a new block volume of the given size.
	// Returns existing volume info if the volume already exists (idempotent).
	CreateVolume(volumeID string, sizeBytes int64) (*VolumeInfo, error)
	// DeleteVolume removes a block volume.
	// Returns nil if the volume does not exist (idempotent).
	DeleteVolume(volumeID string) error
	// GetVolumeInfo returns metadata about an existing volume.
	GetVolumeInfo(volumeID string) (*VolumeInfo, error)
	// VolumeExists checks whether a volume with the given ID exists.
	VolumeExists(volumeID string) (bool, error)
}

// VolumeExpander is an optional interface that providers can implement to
// support online volume expansion (ControllerExpandVolume).
type VolumeExpander interface {
	ExpandVolume(volumeID string, newSizeBytes int64) error
}

// VolumeRecoverer is an optional interface that providers can implement
// to list all volumes they manage (tagged with our CSI tag). This enables
// the volume store to recover state after pod rescheduling.
type VolumeRecoverer interface {
	ListManagedVolumes() ([]*VolumeInfo, error)
}

// VolumeSnapshotter is an optional interface that providers can implement
// to support snapshot creation, deletion, and listing.
type VolumeSnapshotter interface {
	CreateSnapshot(volumeID, snapshotID string) (*SnapshotInfo, error)
	DeleteSnapshot(snapshotID string) error
	ListSnapshots(volumeID string) ([]*SnapshotInfo, error)
}

// VolumeCloner is an optional interface for creating a volume from an
// existing snapshot or another volume.
type VolumeCloner interface {
	CreateVolumeFromSnapshot(volumeID, snapshotID string, sizeBytes int64) (*VolumeInfo, error)
	CreateVolumeFromVolume(volumeID, sourceVolumeID string, sizeBytes int64) (*VolumeInfo, error)
}

// SnapshotInfo holds provider-agnostic metadata about a volume snapshot.
type SnapshotInfo struct {
	SnapshotID     string
	SourceVolumeID string
	SizeBytes      int64
	CreationTime   int64 // Unix timestamp in seconds (UTC)
	ReadyToUse     bool
}
