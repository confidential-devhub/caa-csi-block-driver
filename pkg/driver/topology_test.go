// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"context"
	"errors"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestApplyTopologyParamsFromPreferred(t *testing.T) {
	t.Parallel()

	req := &csi.TopologyRequirement{
		Preferred: []*csi.Topology{{
			Segments: map[string]string{
				topologyZoneKey:   "us-east-2a",
				topologyRegionKey: "us-east-2",
			},
		}},
	}

	params := cloneParams(map[string]string{"cloudProvider": "aws", "awsRegion": "us-east-2"})
	applyTopologyParams(params, req)
	if params["awsAvailabilityZone"] != "us-east-2a" {
		t.Fatalf("expected AZ from preferred topology, got %q", params["awsAvailabilityZone"])
	}
}

func TestApplyTopologyParamsExplicitWins(t *testing.T) {
	t.Parallel()

	req := &csi.TopologyRequirement{
		Preferred: []*csi.Topology{{
			Segments: map[string]string{topologyZoneKey: "us-east-2a"},
		}},
	}
	params := cloneParams(map[string]string{
		"awsRegion":           "us-east-2",
		"awsAvailabilityZone": "us-east-2c",
	})
	applyTopologyParams(params, req)
	if params["awsAvailabilityZone"] != "us-east-2c" {
		t.Fatalf("explicit AZ should win, got %q", params["awsAvailabilityZone"])
	}
}

func TestApplyTopologyParamsWellKnownKey(t *testing.T) {
	t.Parallel()

	req := &csi.TopologyRequirement{
		Requisite: []*csi.Topology{{
			Segments: map[string]string{k8sZoneLabel: "eu-west-1b"},
		}},
	}
	params := cloneParams(nil)
	applyTopologyParams(params, req)
	if params["awsAvailabilityZone"] != "eu-west-1b" {
		t.Fatalf("expected well-known zone label, got %q", params["awsAvailabilityZone"])
	}
}

func TestPreferredWinsOverRequisite(t *testing.T) {
	t.Parallel()

	req := &csi.TopologyRequirement{
		Preferred: []*csi.Topology{{
			Segments: map[string]string{topologyZoneKey: "us-east-2a"},
		}},
		Requisite: []*csi.Topology{{
			Segments: map[string]string{topologyZoneKey: "us-east-2c"},
		}},
	}
	params := cloneParams(nil)
	applyTopologyParams(params, req)
	if params["awsAvailabilityZone"] != "us-east-2a" {
		t.Fatalf("preferred should win, got %q", params["awsAvailabilityZone"])
	}
}

func TestApplyTopologyParamsIgnoresDefaultZone(t *testing.T) {
	t.Parallel()

	req := &csi.TopologyRequirement{
		Preferred: []*csi.Topology{{
			Segments: map[string]string{topologyZoneKey: ignoredTopologyZone},
		}},
	}
	params := cloneParams(nil)
	applyTopologyParams(params, req)
	if params["awsAvailabilityZone"] != "" {
		t.Fatalf("sentinel zone must not become awsAvailabilityZone, got %q", params["awsAvailabilityZone"])
	}
}

func TestAccessibleTopology(t *testing.T) {
	t.Parallel()

	if got := accessibleTopology(map[string]string{"cloudProvider": "libvirt"}); got != nil {
		t.Fatalf("expected nil topology without AZ/region, got %+v", got)
	}

	got := accessibleTopology(map[string]string{
		"awsRegion":           "us-east-2",
		"awsAvailabilityZone": "us-east-2a",
	})
	if len(got) != 1 {
		t.Fatalf("expected 1 topology, got %d", len(got))
	}
	if got[0].Segments[topologyZoneKey] != "us-east-2a" {
		t.Fatalf("unexpected zone segment: %v", got[0].Segments)
	}
	if got[0].Segments[topologyRegionKey] != "us-east-2" {
		t.Fatalf("unexpected region segment: %v", got[0].Segments)
	}
}

func TestResolveNodeTopologyEnvOverride(t *testing.T) {
	t.Setenv("CSI_TOPOLOGY_ZONE", "us-west-2b")
	t.Setenv("CSI_TOPOLOGY_REGION", "us-west-2")

	orig := lookupNodeLabels
	t.Cleanup(func() { lookupNodeLabels = orig })
	lookupNodeLabels = func(context.Context, string) (string, string, error) {
		t.Fatal("env override should not call node label lookup")
		return "", "", nil
	}

	topo := resolveNodeTopology(t.Context(), "node-1")
	if topo == nil {
		t.Fatal("expected topology from env")
	}
	if topo.Segments[topologyZoneKey] != "us-west-2b" || topo.Segments[topologyRegionKey] != "us-west-2" {
		t.Fatalf("unexpected segments: %v", topo.Segments)
	}
}

func TestResolveNodeTopologyFromLabels(t *testing.T) {
	t.Setenv("CSI_TOPOLOGY_ZONE", "")
	t.Setenv("CSI_TOPOLOGY_REGION", "")

	orig := lookupNodeLabels
	t.Cleanup(func() { lookupNodeLabels = orig })
	lookupNodeLabels = func(_ context.Context, nodeID string) (string, string, error) {
		if nodeID != "ip-10-0-1-20" {
			t.Fatalf("unexpected nodeID %q", nodeID)
		}
		return "ap-south-1", "ap-south-1a", nil
	}

	topo := resolveNodeTopology(t.Context(), "ip-10-0-1-20")
	if topo.Segments[topologyZoneKey] != "ap-south-1a" {
		t.Fatalf("unexpected segments: %v", topo.Segments)
	}
}

func TestNodeGetInfoFromEnv(t *testing.T) {
	t.Setenv("CSI_TOPOLOGY_ZONE", "csi-sanity")
	t.Setenv("CSI_TOPOLOGY_REGION", "")

	orig := lookupNodeLabels
	t.Cleanup(func() { lookupNodeLabels = orig })
	lookupNodeLabels = func(context.Context, string) (string, string, error) {
		t.Fatal("CSI_TOPOLOGY_ZONE should not call node label lookup")
		return "", "", nil
	}

	ns := newNodeServer("test-node")
	resp, err := ns.NodeGetInfo(t.Context(), &csi.NodeGetInfoRequest{})
	if err != nil {
		t.Fatalf("NodeGetInfo: %v", err)
	}
	if resp.AccessibleTopology.Segments[topologyZoneKey] != "csi-sanity" {
		t.Fatalf("unexpected segments: %v", resp.AccessibleTopology.Segments)
	}
}

func TestResolveNodeTopologyNoFakeZone(t *testing.T) {
	t.Setenv("CSI_TOPOLOGY_ZONE", "")
	t.Setenv("CSI_TOPOLOGY_REGION", "")

	orig := lookupNodeLabels
	t.Cleanup(func() { lookupNodeLabels = orig })
	lookupNodeLabels = func(context.Context, string) (string, string, error) {
		return "", "", nil
	}

	if topo := resolveNodeTopology(t.Context(), "test-node"); topo != nil {
		t.Fatalf("expected nil topology without a real zone, got %+v", topo)
	}
}

func TestNodeGetInfoUnavailableWithoutZone(t *testing.T) {
	t.Setenv("CSI_TOPOLOGY_ZONE", "")
	t.Setenv("CSI_TOPOLOGY_REGION", "")

	orig := lookupNodeLabels
	t.Cleanup(func() { lookupNodeLabels = orig })
	lookupNodeLabels = func(context.Context, string) (string, string, error) {
		return "", "", nil
	}

	ns := newNodeServer("test-node")
	_, err := ns.NodeGetInfo(t.Context(), &csi.NodeGetInfoRequest{})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("expected Unavailable without a zone, got %v", err)
	}
}

func TestNodeGetInfoRetriesThenCaches(t *testing.T) {
	t.Setenv("CSI_TOPOLOGY_ZONE", "")
	t.Setenv("CSI_TOPOLOGY_REGION", "")

	orig := lookupNodeLabels
	t.Cleanup(func() { lookupNodeLabels = orig })

	calls := 0
	lookupNodeLabels = func(context.Context, string) (string, string, error) {
		calls++
		if calls == 1 {
			return "", "", errors.New("api down")
		}
		return "us-east-2", "us-east-2a", nil
	}

	ns := newNodeServer("test-node")
	if _, err := ns.NodeGetInfo(t.Context(), &csi.NodeGetInfoRequest{}); status.Code(err) != codes.Unavailable {
		t.Fatalf("expected Unavailable on first lookup failure, got %v", err)
	}

	resp, err := ns.NodeGetInfo(t.Context(), &csi.NodeGetInfoRequest{})
	if err != nil {
		t.Fatalf("NodeGetInfo after retry: %v", err)
	}
	if resp.AccessibleTopology.Segments[topologyZoneKey] != "us-east-2a" {
		t.Fatalf("unexpected segments: %v", resp.AccessibleTopology.Segments)
	}

	if _, err := ns.NodeGetInfo(t.Context(), &csi.NodeGetInfoRequest{}); err != nil {
		t.Fatalf("cached NodeGetInfo: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 lookups (fail then success), got %d", calls)
	}
}
