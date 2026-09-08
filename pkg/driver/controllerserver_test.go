// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/confidential-devhub/caa-csi-block-driver/pkg/provider"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/golang/protobuf/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const testGiB int64 = 1024 * 1024 * 1024

type fakeProvisioner struct {
	provider.BlockVolumeProvider
	volume         *provider.VolumeInfo
	calls, creates int
	err            error
}

func (p *fakeProvisioner) NormalizeCapacity(size int64) (int64, error) {
	units := size / testGiB
	if size%testGiB != 0 {
		units++
	}
	if units < 10 {
		units = 10
	}
	return units * testGiB, nil
}

func (p *fakeProvisioner) CreateVolume(name string, size int64) (*provider.VolumeInfo, error) {
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	if p.volume == nil {
		p.creates++
		p.volume = &provider.VolumeInfo{VolumeID: name, Path: "r006-native-id", SizeBytes: size, Provider: "ibmcloud", Metadata: map[string]string{"ibm-volume-id": "r006-native-id", "cloud-volume-path": "r006-native-id", "zone": "us-south-1"}}
	}
	return p.volume, nil
}

func setupProvisioningTest(t *testing.T) (*controllerServer, *fakeProvisioner) {
	t.Helper()
	t.Setenv("CSI_VOLUME_STORE_DIR", t.TempDir())
	t.Setenv("CSI_CLOUD_PROVIDER", "")
	t.Setenv("CLOUD_PROVIDER", "")
	t.Setenv("CSI_BOOTSTRAP_PARAMS_FILE", "")
	p := &fakeProvisioner{}
	provider.RegisterProvider("ibmcloud", func(map[string]string) (provider.BlockVolumeProvider, error) { return p, nil })
	return newControllerServer(), p
}

func createRequest() *csi.CreateVolumeRequest {
	return &csi.CreateVolumeRequest{Name: "pvc-test", Parameters: map[string]string{"cloudProvider": "ibmcloud", "zone": "us-south-1"}, CapacityRange: &csi.CapacityRange{RequiredBytes: 20*testGiB + 1}, VolumeCapabilities: []*csi.VolumeCapability{{AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"}}, AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER}}}}
}

func TestCreateVolumeMetadataAndRestart(t *testing.T) {
	cs, p := setupProvisioningTest(t)
	req := createRequest()
	first, err := cs.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if first.Volume.CapacityBytes != 21*testGiB || first.Volume.VolumeContext["ibm-volume-id"] != "r006-native-id" {
		t.Fatalf("bad response: %+v", first)
	}
	rec, err := cs.store.Load(req.Name)
	if err != nil || rec.Metadata["ibm-volume-id"] != "r006-native-id" || rec.CapacityBytes != 21*testGiB {
		t.Fatalf("bad record: %+v %v", rec, err)
	}
	for _, server := range []*controllerServer{cs, newControllerServer()} {
		got, err := server.CreateVolume(context.Background(), req)
		if err != nil || !proto.Equal(first, got) {
			t.Fatalf("retry changed response: %+v %v", got, err)
		}
	}
	// Simulate pod replacement losing emptyDir, while the cloud disk remains.
	t.Setenv("CSI_VOLUME_STORE_DIR", t.TempDir())
	got, err := newControllerServer().CreateVolume(context.Background(), req)
	if err != nil || !proto.Equal(first, got) || p.creates != 1 || p.calls != 4 {
		t.Fatalf("lost-state retry: %+v %v creates=%d calls=%d", got, err, p.creates, p.calls)
	}
}

func TestCreateVolumeLimitBeforeCloudCall(t *testing.T) {
	cs, p := setupProvisioningTest(t)
	req := createRequest()
	req.CapacityRange.LimitBytes = req.CapacityRange.RequiredBytes
	if _, err := cs.CreateVolume(context.Background(), req); status.Code(err) != codes.OutOfRange {
		t.Fatal(err)
	}
	if p.calls != 0 {
		t.Fatal("cloud called despite rounded size exceeding limit")
	}
}

func TestCreateVolumeInvalidAndConflictingRequests(t *testing.T) {
	for _, change := range []func(*csi.CreateVolumeRequest){
		func(r *csi.CreateVolumeRequest) { r.Name = "../escape" },
		func(r *csi.CreateVolumeRequest) { r.Name = "_manifest" },
		func(r *csi.CreateVolumeRequest) { r.CapacityRange.RequiredBytes = -1 },
		func(r *csi.CreateVolumeRequest) { r.CapacityRange.LimitBytes = 1 },
		func(r *csi.CreateVolumeRequest) { r.VolumeCapabilities = nil },
		func(r *csi.CreateVolumeRequest) { r.Parameters = nil },
	} {
		cs, p := setupProvisioningTest(t)
		req := createRequest()
		change(req)
		if _, err := cs.CreateVolume(context.Background(), req); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("got %v", err)
		}
		if p.calls != 0 {
			t.Fatal("invalid request reached cloud")
		}
	}
	cs, p := setupProvisioningTest(t)
	req := createRequest()
	if _, err := cs.CreateVolume(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	req.Parameters["zone"] = "us-south-2"
	p.err = provider.ErrVolumeAlreadyExists // Live provider detects canonical mismatch.
	if _, err := cs.CreateVolume(context.Background(), req); status.Code(err) != codes.AlreadyExists {
		t.Fatal(err)
	}
	if p.calls != 2 {
		t.Fatal("IBM retry did not validate live settings")
	}
}

func TestRecoveredCredentialsDoNotConflict(t *testing.T) {
	cs, _ := setupProvisioningTest(t)
	params := map[string]string{"cloudProvider": "aws", "awsRegion": "us-south", "awsAccessKeyId": "test-key", "awsSecretKey": "test-secret"}
	rec := &volumeRecord{VolumeID: "pvc-test", Provider: "aws", Path: "native-id", CapacityBytes: 21 * testGiB, Params: sanitizePersistableParams(params)}
	if err := cs.store.Save(rec); err != nil {
		t.Fatal(err)
	}
	req := createRequest()
	req.Parameters = params
	got, err := cs.CreateVolume(context.Background(), req)
	if err != nil || got.Volume.VolumeId != rec.VolumeID {
		t.Fatalf("credential-only difference rejected: %v", err)
	}
	if got.Volume.VolumeContext["awsSecretKey"] != "" {
		t.Fatal("credential added to response")
	}
	req.Parameters["awsRegion"] = "different-region"
	if _, err := cs.CreateVolume(context.Background(), req); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("changed setting accepted: %v", err)
	}
}

func TestIBMParameterAliasesReachLiveValidation(t *testing.T) {
	cs, p := setupProvisioningTest(t)
	req := createRequest()
	if _, err := cs.CreateVolume(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	delete(req.Parameters, "zone")
	req.Parameters["ibmZone"] = "us-south-1"
	if _, err := cs.CreateVolume(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if p.calls != 2 {
		t.Fatal("live validation skipped")
	}
}

func TestCreateVolumePersistenceFailureRetries(t *testing.T) {
	cs, p := setupProvisioningTest(t)
	// A nonexistent directory permits lookup=NotFound but makes Save fail.
	cs.store.dir = filepath.Join(t.TempDir(), "missing")
	if _, err := cs.CreateVolume(context.Background(), createRequest()); status.Code(err) != codes.Internal {
		t.Fatal(err)
	}
	if p.creates != 1 {
		t.Fatal("expected disk creation before persistence error")
	}
	if err := os.Mkdir(cs.store.dir, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CreateVolume(context.Background(), createRequest()); err != nil {
		t.Fatal(err)
	}
	if p.creates != 1 {
		t.Fatal("retry duplicated volume")
	}
}

func TestCreateVolumeCorruptRecordDoesNotCreate(t *testing.T) {
	cs, p := setupProvisioningTest(t)
	if err := os.WriteFile(filepath.Join(cs.store.dir, "pvc-test.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CreateVolume(context.Background(), createRequest()); status.Code(err) != codes.Internal {
		t.Fatal(err)
	}
	if p.calls != 0 {
		t.Fatal("corrupt record ignored")
	}
}

func TestProvisioningErrorCodes(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code codes.Code
	}{{provider.ErrVolumeAlreadyExists, codes.AlreadyExists}, {provider.ErrInvalidParameters, codes.InvalidArgument}, {errors.New("authentication failed"), codes.Internal}} {
		cs, p := setupProvisioningTest(t)
		p.err = tc.err
		if _, err := cs.CreateVolume(context.Background(), createRequest()); status.Code(err) != tc.code {
			t.Fatalf("got %v", err)
		}
	}
}

func TestConcurrentCreateRequests(t *testing.T) {
	cs, p := setupProvisioningTest(t)
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := cs.CreateVolume(context.Background(), createRequest()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if p.creates != 1 {
		t.Fatalf("created %d disks", p.creates)
	}
}
