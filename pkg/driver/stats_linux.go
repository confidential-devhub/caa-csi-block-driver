// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package driver

import (
	"syscall"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

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
