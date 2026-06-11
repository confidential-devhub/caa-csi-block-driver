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
type BlockVolumeProvider interface {
	CreateVolume(volumeID string, sizeBytes int64) (*VolumeInfo, error)
	DeleteVolume(volumeID string) error
	GetVolumeInfo(volumeID string) (*VolumeInfo, error)
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
	CreationTime   int64
	ReadyToUse     bool
}
