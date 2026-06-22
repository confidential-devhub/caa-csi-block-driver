// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type identityServer struct {
	csi.UnimplementedIdentityServer
	driverName    string
	vendorVersion string
}

func (ids *identityServer) GetPluginInfo(_ context.Context, _ *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	if ids.driverName == "" {
		return nil, status.Error(codes.Unavailable, "driver name not configured")
	}
	if ids.vendorVersion == "" {
		return nil, status.Error(codes.Unavailable, "driver version not configured")
	}
	return &csi.GetPluginInfoResponse{
		Name:          ids.driverName,
		VendorVersion: ids.vendorVersion,
	}, nil
}

func (ids *identityServer) GetPluginCapabilities(_ context.Context, _ *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	return &csi.GetPluginCapabilitiesResponse{
		Capabilities: []*csi.PluginCapability{
			{
				Type: &csi.PluginCapability_Service_{
					Service: &csi.PluginCapability_Service{
						Type: csi.PluginCapability_Service_CONTROLLER_SERVICE,
					},
				},
			},
			{
				Type: &csi.PluginCapability_VolumeExpansion_{
					VolumeExpansion: &csi.PluginCapability_VolumeExpansion{
						Type: csi.PluginCapability_VolumeExpansion_ONLINE,
					},
				},
			},
		},
	}, nil
}

func (ids *identityServer) Probe(_ context.Context, _ *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{}, nil
}
