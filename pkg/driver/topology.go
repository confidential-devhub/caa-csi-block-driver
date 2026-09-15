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

var (
	topologyZoneKeys   = []string{topologyZoneKey, k8sZoneLabel, k8sZoneLabelBeta, "ibm-cloud.kubernetes.io/zone"}
	topologyRegionKeys = []string{topologyRegionKey, k8sRegionLabel, k8sRegionLabelBeta, "ibm-cloud.kubernetes.io/region"}
)

func cloneParams(in map[string]string) map[string]string {
	out := make(map[string]string, len(in)+2)
	for k, v := range in {
		out[k] = v
	}
	return out
}

// applyTopologyParams fills awsAvailabilityZone or zone from AccessibilityRequirements
// when the StorageClass did not set it explicitly. Only applies to AWS and IBM Cloud.
func applyTopologyParams(params map[string]string, req *csi.TopologyRequirement) {
	provider := params["cloudProvider"]
	if provider != "aws" && provider != "ibmcloud" {
		return
	}

	zone := topologyValue(req, topologyZoneKeys)
	if zone == "" || zone == ignoredTopologyZone {
		return
	}

	if provider == "aws" {
		if params["awsAvailabilityZone"] == "" {
			params["awsAvailabilityZone"] = zone
		}
	} else if provider == "ibmcloud" {
		if params["zone"] == "" && params["ibmZone"] == "" {
			params["zone"] = zone
		}
		if params["region"] == "" && params["ibmRegion"] == "" {
			region := topologyValue(req, topologyRegionKeys)
			if region != "" {
				params["region"] = region
			}
		}
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
	var zone, region string

	if params["awsAvailabilityZone"] != "" {
		zone = params["awsAvailabilityZone"]
		region = params["awsRegion"]
	} else if params["cloudProvider"] == "ibmcloud" {
		zone = params["zone"]
		if zone == "" {
			zone = params["ibmZone"]
		}
		region = params["region"]
		if region == "" {
			region = params["ibmRegion"]
		}
	}

	if zone == "" {
		return nil
	}
	segments := map[string]string{topologyZoneKey: zone}
	if region != "" {
		segments[topologyRegionKey] = region
	}
	return []*csi.Topology{{Segments: segments}}
}
