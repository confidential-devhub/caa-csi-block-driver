// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"github.com/container-storage-interface/spec/lib/go/csi"
)

const (
	topologyZoneKey   = "topology.caa-csi.io/zone"
	topologyRegionKey = "topology.caa-csi.io/region"

	// ignoredTopologyZone is not a real AWS AZ and is never copied into
	// awsAvailabilityZone.
	ignoredTopologyZone = "default"

	k8sZoneLabel       = "topology.kubernetes.io/zone"
	k8sRegionLabel     = "topology.kubernetes.io/region"
	k8sZoneLabelBeta   = "failure-domain.beta.kubernetes.io/zone"
	k8sRegionLabelBeta = "failure-domain.beta.kubernetes.io/region"
)

var topologyZoneKeys = []string{topologyZoneKey, k8sZoneLabel, k8sZoneLabelBeta}

func cloneParams(in map[string]string) map[string]string {
	out := make(map[string]string, len(in)+2)
	for k, v := range in {
		out[k] = v
	}
	return out
}

// applyTopologyParams fills awsAvailabilityZone from AccessibilityRequirements
// when the StorageClass did not set it explicitly. Only applies to AWS —
// other providers don't use awsAvailabilityZone and shouldn't have it
// injected into their volume records or VolumeContext.
func applyTopologyParams(params map[string]string, req *csi.TopologyRequirement) {
	if params["cloudProvider"] != "aws" {
		return
	}
	if params["awsAvailabilityZone"] != "" {
		return
	}
	if zone := topologyValue(req, topologyZoneKeys); zone != "" && zone != ignoredTopologyZone {
		params["awsAvailabilityZone"] = zone
	}
}

func topologyValue(req *csi.TopologyRequirement, keys []string) string {
	if req == nil {
		return ""
	}
	if v := firstSegment(req.GetPreferred(), keys); v != "" {
		return v
	}
	return firstSegment(req.GetRequisite(), keys)
}

func firstSegment(topos []*csi.Topology, keys []string) string {
	for _, topo := range topos {
		if topo == nil {
			continue
		}
		segs := topo.GetSegments()
		for _, key := range keys {
			if v := segs[key]; v != "" {
				return v
			}
		}
	}
	return ""
}

func accessibleTopology(params map[string]string) []*csi.Topology {
	if params == nil {
		return nil
	}
	zone := params["awsAvailabilityZone"]
	if zone == "" {
		return nil
	}
	segments := map[string]string{topologyZoneKey: zone}
	if region := params["awsRegion"]; region != "" {
		segments[topologyRegionKey] = region
	}
	return []*csi.Topology{{Segments: segments}}
}
