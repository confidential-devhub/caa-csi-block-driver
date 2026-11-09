// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package driver

import (
	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// getVolumeStatsLocal is only supported on Linux; this stub lets the package
// compile (and its unit tests run) on other development platforms.
func getVolumeStatsLocal(_ string) (*csi.NodeGetVolumeStatsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "host-side volume stats are only supported on Linux")
}
