// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package driver

import "testing"

func TestDeleteProviderParams(t *testing.T) {
	rec := &volumeRecord{
		Path: "r006-native-volume-id",
		Params: map[string]string{
			"cloudProvider":     "ibmcloud",
			"cloud-volume-path": "stale-id",
		},
	}

	params := deleteProviderParams(rec)
	if got := params["cloud-volume-path"]; got != rec.Path {
		t.Fatalf("cloud-volume-path = %q, want %q", got, rec.Path)
	}
	if got := rec.Params["cloud-volume-path"]; got != "stale-id" {
		t.Fatalf("stored params were mutated: cloud-volume-path = %q", got)
	}
}

func TestDeleteProviderParamsWithoutPath(t *testing.T) {
	rec := &volumeRecord{
		Params: map[string]string{"cloudProvider": "ibmcloud"},
	}

	params := deleteProviderParams(rec)
	if _, ok := params["cloud-volume-path"]; ok {
		t.Fatal("cloud-volume-path should not be added when the stored path is empty")
	}
}
