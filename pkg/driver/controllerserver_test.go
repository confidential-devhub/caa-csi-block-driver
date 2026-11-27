// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

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

func TestCreateSnapshotValidation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CSI_VOLUME_STORE_DIR", dir)
	t.Setenv("CSI_CLOUD_PROVIDER", "")
	t.Setenv("CSI_BOOTSTRAP_PARAMS_FILE", "")

	cs := &controllerServer{
		store: newVolumeStore(),
	}

	// Missing source volume ID
	_, err := cs.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{
		Name: "snap-1",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for missing source volume, got %v", err)
	}

	// Missing snapshot name
	_, err = cs.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{
		SourceVolumeId: "pvc-1",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for missing snapshot name, got %v", err)
	}

	// Source volume not found and no provider params
	_, err = cs.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{
		SourceVolumeId: "pvc-missing",
		Name:           "snap-1",
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound for non-existent volume, got %v", err)
	}
}
