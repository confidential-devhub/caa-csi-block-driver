// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package ibmcloud

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/IBM/ibmcloud-volume-interface/lib/provider"
	providerError "github.com/IBM/ibmcloud-volume-interface/lib/utils"
)

type fakeSession struct {
	provider.Session
	volumes     map[string]*provider.Volume
	lookupErr   error
	createErr   error
	deleteErr   error
	createCalls int
	deleteCalls int
	lastCreate  provider.Volume
	lastDelete  string
}

func newFakeSession() *fakeSession {
	return &fakeSession{volumes: make(map[string]*provider.Volume)}
}

func TestActualSDKConversionLookup(t *testing.T) {
	p, s := testProvider(t)
	v := vpcProvider.FromProviderToLibVolume(&models.Volume{
		ID: "r006-native", Name: "pvc-test", Capacity: 20, Status: "available",
		Zone: &models.Zone{Name: "us-south-1"}, Profile: &models.Profile{Name: "general-purpose"},
		UserTags: []string{ownershipTagPrefix + "pvc-test", p.configTag()},
	}, zap.NewNop())
	s.pages = map[string]*provider.VolumeList{"": {Volumes: []*provider.Volume{v}}}
	info, err := p.CreateVolume(context.TODO(), "pvc-test", 20*gib)
	if err != nil || info.VolumeID != "pvc-test" || info.Path != "r006-native" || s.creates != 0 {
		t.Fatalf("actual SDK conversion: %+v %v", info, err)
	}
	if volume, ok := s.volumes[name]; ok {
		return volume, nil
	}
	return nil, providerError.Message{Type: providerError.RetrivalFailed, RC: 404}
}

func TestSameNameOutsideRequestedPlacementIsConflict(t *testing.T) {
	for _, field := range []string{"zone", "group"} {
		t.Run(field, func(t *testing.T) {
			p, s := testProvider(t)
			if _, err := p.CreateVolume(context.TODO(), "pvc-test", 20*gib); err != nil {
				t.Fatal(err)
			}
			if field == "zone" {
				p.config.Zone = "us-south-2"
			} else {
				p.config.ResourceGroup = "other-group"
			}
			if _, err := p.CreateVolume(context.TODO(), "pvc-test", 20*gib); !errors.Is(err, caa.ErrVolumeAlreadyExists) {
				t.Fatalf("hidden existing disk: %v", err)
			}
			if err := p.DeleteVolume(context.TODO(), "pvc-test"); !errors.Is(err, caa.ErrVolumeAlreadyExists) {
				t.Fatalf("must not report successful deletion: %v", err)
			}
			if s.creates != 1 || s.deletes != 0 {
				t.Fatalf("unexpected mutation: creates=%d deletes=%d", s.creates, s.deletes)
			}
		})
	}
}

func TestDeleteDoesNotRequireAvailableVolume(t *testing.T) {
	for _, state := range []string{"failed", "pending", "deleting"} {
		t.Run(state, func(t *testing.T) {
			p, s := testProvider(t)
			if _, err := p.CreateVolume(context.TODO(), "pvc-test", 20*gib); err != nil {
				t.Fatal(err)
			}
			s.volumes[0].Status = state
			s.volumes[0].Capacity = nil
			if err := p.DeleteVolume(context.TODO(), "pvc-test"); err != nil {
				t.Fatal(err)
			}
			if s.deletes != 1 {
				t.Fatal("cloud deletion was not attempted")
			}
		})
	}
}

func (s *fakeSession) CreateVolume(v provider.Volume) (*provider.Volume, error) {
	s.creates++
	if s.createErr != nil {
		return nil, s.createErr
	}
	volume.VolumeID = "r006-created-volume"
	if volume.Name != nil {
		s.volumes[*volume.Name] = &volume
	}
	return &volume, nil
}

func (s *fakeSession) DeleteVolume(volume *provider.Volume) error {
	s.deleteCalls++
	if volume != nil {
		s.lastDelete = volume.VolumeID
	}
	if s.deleteErr != nil {
		return s.deleteErr
	}
	for name, existing := range s.volumes {
		if existing.VolumeID == s.lastDelete {
			delete(s.volumes, name)
		}
	}
	return nil
}

func (s *fakeSession) UpdateVolume(volume provider.Volume) error {
	for _, existing := range s.volumes {
		if existing.VolumeID == volume.VolumeID {
			existing.Capacity = volume.Capacity
			return nil
		}
	}
	return fmt.Errorf("volume not found")
}

func (s *fakeSession) ListVolumes(_ int, _ string, _ map[string]string) (*provider.VolumeList, error) {
	list := &provider.VolumeList{}
	for _, volume := range s.volumes {
		list.Volumes = append(list.Volumes, volume)
	}
	existing.Capacity = v.Capacity
	return nil
}

func TestCustomCreateRequestAndExistingExpansion(t *testing.T) {
	p, s := testProvider(t)
	params := testParams()
	params["profile"] = "custom"
	params["iops"] = "05000"
	params["encrypted"] = "true"
	params["encryptionKey"] = "crn:test-key"
	cfg, err := parseConfig(params)
	if err != nil {
		t.Fatal(err)
	}
	p.config = cfg
	if _, err := p.CreateVolume(context.TODO(), "pvc-test", 20*gib); err != nil {
		t.Fatal(err)
	}
	v := s.volumes[0]
	if *v.Iops != "5000" || v.ResourceGroup.ID != "rg-test" || v.VolumeEncryptionKey.CRN != "crn:test-key" {
		t.Fatalf("wrong request: %+v", v)
	}
	if _, err := p.CreateVolume(context.TODO(), "pvc-test", 20*gib); err != nil {
		t.Fatal(err)
	}
	// Retain the existing provider-level expansion regression coverage; no
	// controller/guest expansion support is added by this provisioning change.
	if err := p.ExpandVolume(context.TODO(), "pvc-test", 40*gib); err != nil {
		t.Fatal(err)
	}
	info, err := p.GetVolumeInfo(context.TODO(), "pvc-test")
	if err != nil || info.SizeBytes != 40*gib {
		t.Fatalf("expanded result: %+v %v", info, err)
	}
	p.config.Iops = "6000"
	if _, err := p.CreateVolume(context.TODO(), "pvc-test", 20*gib); !errors.Is(err, caa.ErrVolumeAlreadyExists) {
		t.Fatalf("configuration mismatch: %v", err)
	}
}

func TestCreateFailureAndDuplicateName(t *testing.T) {
	p, s := testProvider(t)
	failure := errors.New("create API failure")
	s.createErr = failure
	if _, err := p.CreateVolume(context.TODO(), "pvc-test", 20*gib); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	s.createErr = nil
	if _, err := p.CreateVolume(context.TODO(), "pvc-test", 20*gib); err != nil {
		t.Fatal(err)
	}
	s.volumes = append(s.volumes, s.volumes[0])
	if _, err := p.CreateVolume(context.TODO(), "pvc-test", 20*gib); !errors.Is(err, caa.ErrVolumeAlreadyExists) {
		t.Fatalf("ambiguous lookup: %v", err)
	}
}

func TestPinnedSDKResponseShape(t *testing.T) {
	p, s := testProvider(t)
	s.createResult = func(v provider.Volume) *provider.Volume {
		v.VolumeID = "r006-sdk-shape"
		v.Name = nil
		v.ResourceGroup = nil
		v.VolumeEncryptionKey = nil
		v.Status = "available"
		s.volumes = append(s.volumes, &v)
		pending := v
		pending.Status = "pending"
		return &pending
	}
	first, err := p.CreateVolume(context.TODO(), "pvc-test", 20*gib)
	if err != nil {
		t.Fatal(err)
	}
	// List conversion also omits Name, RG and encryption key.
	s.volumes[0].Name = nil
	s.pages = map[string]*provider.VolumeList{"": {Volumes: s.volumes}}
	second, err := p.CreateVolume(context.TODO(), "pvc-test", 20*gib)
	if err != nil || !reflect.DeepEqual(first, second) || s.creates != 1 {
		t.Fatalf("SDK-shaped retry: %+v %v", second, err)
	}
	s.volumes[0].Name = nil
	recovered, err := p.ListManagedVolumes(context.TODO())
	if err != nil || len(recovered) != 1 || recovered[0].VolumeID != "pvc-test" {
		t.Fatalf("SDK-shaped recovery: %+v %v", recovered, err)
	}
}

func testParams() map[string]string {
	return map[string]string{"region": "us-south", "zone": "us-south-1", "resourceGroup": "rg-test"}
}

func testProvider(t *testing.T) (*IBMCloudProvider, *fakeSession) {
	t.Helper()
	cfg, err := parseConfig(testParams())
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeSession{}
	return &IBMCloudProvider{session: s, config: cfg}, s
}

func TestParseConfig(t *testing.T) {
	t.Run("defaults and aliases", func(t *testing.T) {
		cfg, err := parseConfig(map[string]string{
			"ibmRegion":        "us-south",
			"ibmZone":          "us-south-1",
			"ibmResourceGroup": "rg-alias",
			"tags":             "one, two",
		})
	}
	for _, change := range []map[string]string{
		{"profile": "custom", "iops": "-1"}, {"encrypted": "maybe"},
		{"profile": "sdp", "iops": "0"}, {"profile": "sdp", "iops": "bad"},
		{"throughput": "-1"}, {"throughput": "0"}, {"throughput": "bad"}, {"throughput": "2147483648"},
		{"encrypted": "true"}, {"encryptionKey": "key"}, {"tags": ownershipTagPrefix + "someone-else"},
	} {
		params := testParams()
		for k, v := range change {
			params[k] = v
		}
		if _, err := parseConfig(params); err == nil {
			t.Errorf("accepted invalid config %v", change)
		}
	}
}

func TestResolveResourceGroup(t *testing.T) {
	cfg, err := resolveResourceGroup("storage-class-rg", "provider-rg")
	if err != nil || cfg != "storage-class-rg" {
		t.Fatalf("explicit resource group lost: %q %v", cfg, err)
	}
	cfg, err = resolveResourceGroup("", "provider-rg")
	if err != nil || cfg != "provider-rg" {
		t.Fatalf("provider resource group not used: %q %v", cfg, err)
	}
	if _, err := resolveResourceGroup("", ""); !errors.Is(err, caa.ErrInvalidParameters) {
		t.Fatalf("missing provider default: %v", err)
	}
}

func TestNormalizeCapacity(t *testing.T) {
	p, _ := testProvider(t)
	for _, tc := range []struct{ in, want int64 }{{0, 10 * gib}, {1, 10 * gib}, {10 * gib, 10 * gib}, {20*gib + 1, 21 * gib}, {20 * gib, 20 * gib}} {
		got, err := p.NormalizeCapacity(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("%d: got %d, %v", tc.in, got, err)
		}
	}
	for _, in := range []int64{-1, math.MaxInt64} {
		if _, err := p.NormalizeCapacity(in); err == nil {
			t.Errorf("accepted %d", in)
		}
	}
	p.config.Profile = "sdp"
	for _, tc := range []struct{ in, want int64 }{{0, gib}, {1, gib}, {gib, gib}, {gib + 1, 2 * gib}} {
		got, err := p.NormalizeCapacity(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("SDP %d: got %d, %v", tc.in, got, err)
		}
	}
}

func TestCreateRetryAndDelete(t *testing.T) {
	p, s := testProvider(t)
	first, err := p.CreateVolume(context.TODO(), "pvc-test", 20*gib+1)
	if err != nil {
		t.Fatal(err)
	}
	if first.SizeBytes != 21*gib || first.Metadata["ibm-volume-id"] != first.Path {
		t.Fatalf("bad result: %+v", first)
	}
	restarted := &IBMCloudProvider{session: s, config: p.config}
	second, err := restarted.CreateVolume(context.TODO(), "pvc-test", 20*gib+1)
	if err != nil || !reflect.DeepEqual(first, second) || s.creates != 1 {
		t.Fatalf("retry: %+v %v creates=%d", second, err, s.creates)
	}
	if _, err := p.CreateVolume(context.TODO(), "pvc-test", 22*gib); !errors.Is(err, caa.ErrVolumeAlreadyExists) {
		t.Fatalf("larger retry: %v", err)
	}
	if err := p.DeleteVolume(context.TODO(), "pvc-test"); err != nil {
		t.Fatal(err)
	}
	if err := p.DeleteVolume(context.TODO(), "pvc-test"); err != nil || s.deletes != 1 {
		t.Fatalf("delete retry: %v count=%d", err, s.deletes)
	}
}

func TestLookupFailuresDoNotCreateOrDelete(t *testing.T) {
	for _, message := range []string{"forbidden", "timeout", "connection refused"} {
		t.Run(message, func(t *testing.T) {
			p, s := testProvider(t)
			failure := errors.New(message)
			s.listErr = failure
			if _, err := p.CreateVolume(context.TODO(), "pvc-test", 20*gib); !errors.Is(err, failure) {
				t.Fatal(err)
			}
			if _, err := p.VolumeExists(context.TODO(), "pvc-test"); !errors.Is(err, failure) {
				t.Fatal(err)
			}
			if err := p.DeleteVolume(context.TODO(), "pvc-test"); !errors.Is(err, failure) {
				t.Fatal(err)
			}
			if s.creates != 0 || s.deletes != 0 {
				t.Fatal("mutated after lookup failure")
			}
		})
	}
}

func TestRejectForeignOrIncompatibleVolume(t *testing.T) {
	for name, change := range map[string]func(*provider.Volume){
		"untagged":  func(v *provider.Volume) { v.Tags = nil },
		"wrong-tag": func(v *provider.Volume) { v.Tags = []string{ownershipTagPrefix + "other"} },
		"zone":      func(v *provider.Volume) { v.Az = "us-south-2" },
		"group":     func(v *provider.Volume) { v.ResourceGroup = &provider.ResourceGroup{ID: "other"} },
		"profile":   func(v *provider.Volume) { v.Profile = &provider.Profile{Name: "custom"} },
		"key":       func(v *provider.Volume) { v.VolumeEncryptionKey = &provider.VolumeEncryptionKey{CRN: "other"} },
	} {
		t.Run(name, func(t *testing.T) {
			p, s := testProvider(t)
			if _, err := p.CreateVolume(context.TODO(), "pvc-test", 20*gib); err != nil {
				t.Fatal(err)
			}
			change(s.volumes[0])
			if _, err := p.CreateVolume(context.TODO(), "pvc-test", 20*gib); !errors.Is(err, caa.ErrVolumeAlreadyExists) {
				t.Fatalf("got %v", err)
			}
			if err := p.DeleteVolume(context.TODO(), "pvc-test"); !errors.Is(err, caa.ErrVolumeAlreadyExists) {
				t.Fatalf("delete: %v", err)
			}
			if s.creates != 1 || s.deletes != 0 {
				t.Fatal("mutated incompatible disk")
			}
		})
	}
}

func TestCreateVolumeRequestMapping(t *testing.T) {
	tests := []struct {
		name               string
		config             Config
		size               int64
		wantSizeGB         int
		wantIOPS           string
		wantBandwidth      int32
		wantResourceGroup  string
		wantEncryptionKey  string
		wantFilesystemType provider.VolumeType
	}{
		{
			name: "general purpose",
			config: Config{
				Region: "us-south", Zone: "us-south-1", Profile: "general-purpose",
				ResourceGroup: "rg-explicit", Iops: "3000", ExtraTags: []string{"test"},
				Encrypted: "true", EncryptionKey: "crn:key", FsType: "ext4",
			},
			size:               20*gib + 1,
			wantSizeGB:         21,
			wantResourceGroup:  "rg-explicit",
			wantEncryptionKey:  "crn:key",
			wantFilesystemType: provider.VolumeType("ext4"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			p, s := testProvider(t)
			s.createResult = result
			if _, err := p.CreateVolume(context.TODO(), "pvc-test", 20*gib); err == nil {
				t.Fatal("accepted incomplete result")
			}
		})
	}
}

func TestRecoveryOwnershipAndPagination(t *testing.T) {
	p, s := testProvider(t)
	if _, err := p.CreateVolume(context.TODO(), "pvc-test", 20*gib); err != nil {
		t.Fatal(err)
	}
	owned := s.volumes[0]
	foreign := *owned
	name := "unrelated-volume"
	foreign.Name = &name
	foreign.Tags = nil
	s.pages = map[string]*provider.VolumeList{"": {Volumes: []*provider.Volume{&foreign}, Next: "page2"}, "page2": {Volumes: []*provider.Volume{owned}}}
	vols, err := p.ListManagedVolumes(context.TODO())
	if err != nil || len(vols) != 1 || vols[0].VolumeID != "pvc-test" {
		t.Fatalf("recovery: %+v %v", vols, err)
	}
	s.pages["page2"].Next = "page2"
	if _, err := p.ListManagedVolumes(context.TODO()); err == nil {
		t.Fatal("accepted pagination loop")
	}
	s.pages = map[string]*provider.VolumeList{"": nil}
	if _, err := p.CreateVolume(context.TODO(), "pvc-new", 20*gib); err == nil {
		t.Fatal("nil list treated as absence")
	}
}
