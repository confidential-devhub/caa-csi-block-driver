// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

func TestParseKataVolumeStats(t *testing.T) {
	t.Parallel()

	valid := []byte(`{
		"usage": [
			{"available": 100, "total": 200, "used": 100, "unit": 1},
			{"available": 50, "total": 100, "used": 50, "unit": 2}
		],
		"volume_condition": {"abnormal": false, "message": "OK"}
	}`)

	resp, err := parseKataVolumeStats(valid)
	if err != nil {
		t.Fatalf("expected valid kata stats JSON to parse, got: %v", err)
	}
	if len(resp.Usage) != 2 {
		t.Fatalf("expected 2 usage entries, got %d", len(resp.Usage))
	}
	if resp.Usage[0].Unit != csi.VolumeUsage_BYTES || resp.Usage[0].Total != 200 || resp.Usage[0].Used != 100 {
		t.Fatalf("unexpected bytes usage: %+v", resp.Usage[0])
	}
	if resp.Usage[1].Unit != csi.VolumeUsage_INODES || resp.Usage[1].Total != 100 || resp.Usage[1].Available != 50 {
		t.Fatalf("unexpected inode usage: %+v", resp.Usage[1])
	}

	// Old incorrect flat schema must not silently succeed with zeros.
	flat := []byte(`{"usedBytes":10,"totalBytes":20,"availBytes":10,"usedInodes":1,"totalInodes":2,"freeInodes":1}`)
	if _, err := parseKataVolumeStats(flat); err == nil {
		t.Fatal("expected flat usedBytes schema to be rejected")
	}

	if _, err := parseKataVolumeStats([]byte(`{"usage":[]}`)); err == nil {
		t.Fatal("expected empty usage array to be rejected")
	}

	if _, err := parseKataVolumeStats([]byte(`not-json`)); err == nil {
		t.Fatal("expected invalid JSON to be rejected")
	}
}
