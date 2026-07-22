// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestBootstrapParamsPriority(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CSI_VOLUME_STORE_DIR", dir)
	t.Setenv("CSI_CLOUD_PROVIDER", "")
	t.Setenv("CLOUD_PROVIDER", "")
	t.Setenv("CSI_BOOTSTRAP_PARAMS_FILE", "")

	vs := newVolumeStore()

	// 4) env is used when nothing else is available
	t.Setenv("CSI_CLOUD_PROVIDER", "aws")
	t.Setenv("CSI_AWS_REGION", "us-west-2")
	params := vs.BootstrapParams()
	if params["cloudProvider"] != "aws" || params["awsRegion"] != "us-west-2" {
		t.Fatalf("expected env bootstrap params, got %#v", params)
	}

	// 3) bootstrap file overrides env
	bootFile := filepath.Join(dir, "bootstrap.json")
	if err := os.WriteFile(bootFile, []byte(`{"cloudProvider":"azure","azureSubscriptionId":"sub","azureResourceGroup":"rg"}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CSI_BOOTSTRAP_PARAMS_FILE", bootFile)
	params = vs.BootstrapParams()
	if params["cloudProvider"] != "azure" || params["azureSubscriptionId"] != "sub" {
		t.Fatalf("expected file bootstrap params, got %#v", params)
	}

	// 2) manifest overrides bootstrap file
	manifest := volumeManifest{
		Params: map[string]string{
			"cloudProvider": "aws",
			"awsRegion":     "eu-west-1",
		},
	}
	data, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(dir, manifestFileName), data, 0600); err != nil {
		t.Fatal(err)
	}
	params = vs.BootstrapParams()
	if params["cloudProvider"] != "aws" || params["awsRegion"] != "eu-west-1" {
		t.Fatalf("expected manifest bootstrap params, got %#v", params)
	}

	// 1) surviving volume record overrides manifest
	rec := &volumeRecord{
		VolumeID: "vol-1",
		Provider: "aws",
		Path:     "vol-abc",
		Params: map[string]string{
			"cloudProvider": "aws",
			"awsRegion":     "ap-south-1",
		},
	}
	if err := vs.Save(rec); err != nil {
		t.Fatal(err)
	}
	params = vs.BootstrapParams()
	if params["awsRegion"] != "ap-south-1" {
		t.Fatalf("expected volume record params, got %#v", params)
	}
}

func TestWriteManifestSurvivesLastDelete(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CSI_VOLUME_STORE_DIR", dir)
	t.Setenv("CSI_CLOUD_PROVIDER", "")
	t.Setenv("CSI_BOOTSTRAP_PARAMS_FILE", "")

	vs := newVolumeStore()
	if err := vs.Save(&volumeRecord{
		VolumeID: "vol-1",
		Provider: "aws",
		Path:     "vol-abc",
		Params: map[string]string{
			"cloudProvider":        "aws",
			"awsRegion":            "us-east-2",
			"awsAvailabilityZone":  "us-east-2a",
		},
	}); err != nil {
		t.Fatal(err)
	}

	vs.Delete("vol-1")

	params := vs.BootstrapParams()
	if params == nil || params["cloudProvider"] != "aws" || params["awsRegion"] != "us-east-2" {
		t.Fatalf("expected params-only manifest after last delete, got %#v", params)
	}

	// Volume record itself must be gone
	exists, err := vs.Exists("vol-1")
	if err != nil {
		t.Fatalf("Exists after delete: %v", err)
	}
	if exists {
		t.Fatal("volume record should be deleted")
	}
}

func TestRecoverFromCloudSkipsExisting(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CSI_VOLUME_STORE_DIR", dir)

	vs := newVolumeStore()
	if err := vs.Save(&volumeRecord{
		VolumeID:      "keep-me",
		Provider:      "aws",
		Path:          "original-path",
		CapacityBytes: 1,
		Params: map[string]string{
			"cloudProvider": "aws",
			"awsRegion":     "us-east-1",
		},
	}); err != nil {
		t.Fatal(err)
	}

	// RecoverFromCloud with nil/empty provider is a no-op
	if err := vs.RecoverFromCloud(nil); err != nil {
		t.Fatalf("nil params should be no-op: %v", err)
	}
	if err := vs.RecoverFromCloud(map[string]string{}); err != nil {
		t.Fatalf("empty params should be no-op: %v", err)
	}

	rec, err := vs.Load("keep-me")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Path != "original-path" {
		t.Fatalf("existing record was modified: %#v", rec)
	}
}

func TestParamsFromEnvAWSAndAzure(t *testing.T) {
	t.Setenv("CSI_CLOUD_PROVIDER", "aws")
	t.Setenv("AWS_REGION", "us-east-2")
	t.Setenv("CSI_AWS_AVAILABILITY_ZONE", "us-east-2c")
	params := paramsFromEnv()
	if params["cloudProvider"] != "aws" || params["awsRegion"] != "us-east-2" || params["awsAvailabilityZone"] != "us-east-2c" {
		t.Fatalf("unexpected aws env params: %#v", params)
	}

	t.Setenv("CSI_CLOUD_PROVIDER", "azure")
	t.Setenv("AZURE_SUBSCRIPTION_ID", "sub-1")
	t.Setenv("CSI_AZURE_RESOURCE_GROUP", "rg-1")
	t.Setenv("CSI_AZURE_LOCATION", "eastus")
	params = paramsFromEnv()
	if params["azureSubscriptionId"] != "sub-1" || params["azureResourceGroup"] != "rg-1" || params["azureLocation"] != "eastus" {
		t.Fatalf("unexpected azure env params: %#v", params)
	}
}

func TestLoadBootstrapParamsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "params.json")
	if err := os.WriteFile(path, []byte(`{"cloudProvider":"libvirt","cloudProviderVolumePath":"/var/lib/libvirt/images"}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CSI_BOOTSTRAP_PARAMS_FILE", path)
	params := loadBootstrapParamsFile()
	if params["cloudProvider"] != "libvirt" || params["cloudProviderVolumePath"] == "" {
		t.Fatalf("unexpected file params: %#v", params)
	}
}
