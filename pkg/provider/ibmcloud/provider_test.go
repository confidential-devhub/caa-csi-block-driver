// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package ibmcloud

import (
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/IBM/ibmcloud-volume-interface/lib/provider"
	vpcProvider "github.com/IBM/ibmcloud-volume-vpc/block/provider"
	"github.com/IBM/ibmcloud-volume-vpc/common/vpcclient/models"
	caa "github.com/confidential-devhub/caa-csi-block-driver/pkg/provider"
	"go.uber.org/zap"
)

type fakeSession struct {
	provider.Session
	volumes            []*provider.Volume
	listErr, createErr error
	createResult       func(provider.Volume) *provider.Volume
	pages              map[string]*provider.VolumeList
	creates, deletes   int
	lastFilters        map[string]string
}

func (s *fakeSession) ListVolumes(_ int, start string, filters map[string]string) (*provider.VolumeList, error) {
	s.lastFilters = filters
	if s.listErr != nil {
		return nil, s.listErr
	}
	if s.pages != nil {
		return s.pages[start], nil
	}
	page := &provider.VolumeList{}
	for _, v := range s.volumes {
		if filters["name"] != "" && (v.Name == nil || *v.Name != filters["name"]) {
			continue
		}
		if filters["zone.name"] != "" && v.Az != filters["zone.name"] {
			continue
		}
		if filters["resource_group.id"] != "" && (v.ResourceGroup == nil || v.ResourceGroup.ID != filters["resource_group.id"]) {
			continue
		}
		page.Volumes = append(page.Volumes, v)
	}
	return page, nil
}

func TestActualSDKConversionLookup(t *testing.T) {
	p, s := testProvider(t)
	v := vpcProvider.FromProviderToLibVolume(&models.Volume{
		ID: "r006-native", Name: "pvc-test", Capacity: 20, Status: "available",
		Zone: &models.Zone{Name: "us-south-1"}, Profile: &models.Profile{Name: "general-purpose"},
		UserTags: []string{ownershipTagPrefix + "pvc-test", p.configTag()},
	}, zap.NewNop())
	s.pages = map[string]*provider.VolumeList{"": {Volumes: []*provider.Volume{v}}}
	info, err := p.CreateVolume("pvc-test", 20*gib)
	if err != nil || info.VolumeID != "pvc-test" || info.Path != "r006-native" || s.creates != 0 {
		t.Fatalf("actual SDK conversion: %+v %v", info, err)
	}
	if s.lastFilters["name"] != "pvc-test" || len(s.lastFilters) != 1 {
		t.Fatalf("missing server-side identity filters: %v", s.lastFilters)
	}
}

func TestSameNameOutsideRequestedPlacementIsConflict(t *testing.T) {
	for _, field := range []string{"zone", "group"} {
		t.Run(field, func(t *testing.T) {
			p, s := testProvider(t)
			if _, err := p.CreateVolume("pvc-test", 20*gib); err != nil {
				t.Fatal(err)
			}
			if field == "zone" {
				p.config.Zone = "us-south-2"
			} else {
				p.config.ResourceGroup = "other-group"
			}
			if _, err := p.CreateVolume("pvc-test", 20*gib); !errors.Is(err, caa.ErrVolumeAlreadyExists) {
				t.Fatalf("hidden existing disk: %v", err)
			}
			if err := p.DeleteVolume("pvc-test"); !errors.Is(err, caa.ErrVolumeAlreadyExists) {
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
			if _, err := p.CreateVolume("pvc-test", 20*gib); err != nil {
				t.Fatal(err)
			}
			s.volumes[0].Status = state
			s.volumes[0].Capacity = nil
			if err := p.DeleteVolume("pvc-test"); err != nil {
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
	if s.createResult != nil {
		return s.createResult(v), nil
	}
	v.VolumeID = "r006-test-volume"
	v.Status = "available"
	s.volumes = append(s.volumes, &v)
	return &v, nil
}

func (s *fakeSession) DeleteVolume(v *provider.Volume) error {
	s.deletes++
	for i, existing := range s.volumes {
		if existing.VolumeID == v.VolumeID {
			s.volumes = append(s.volumes[:i], s.volumes[i+1:]...)
			break
		}
	}
	return nil
}

func (s *fakeSession) GetVolume(id string) (*provider.Volume, error) {
	for _, v := range s.volumes {
		if v.VolumeID == id {
			return v, nil
		}
	}
	return nil, errors.New("not found")
}

func (s *fakeSession) UpdateVolume(v provider.Volume) error {
	existing, err := s.GetVolume(v.VolumeID)
	if err != nil {
		return err
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
	if _, err := p.CreateVolume("pvc-test", 20*gib); err != nil {
		t.Fatal(err)
	}
	v := s.volumes[0]
	if *v.Iops != "5000" || v.ResourceGroup.ID != "rg-test" || v.VolumeEncryptionKey.CRN != "crn:test-key" {
		t.Fatalf("wrong request: %+v", v)
	}
	if _, err := p.CreateVolume("pvc-test", 20*gib); err != nil {
		t.Fatal(err)
	}
	// Retain the existing provider-level expansion regression coverage; no
	// controller/guest expansion support is added by this provisioning change.
	if err := p.ExpandVolume("pvc-test", 40*gib); err != nil {
		t.Fatal(err)
	}
	info, err := p.GetVolumeInfo("pvc-test")
	if err != nil || info.SizeBytes != 40*gib {
		t.Fatalf("expanded result: %+v %v", info, err)
	}
	p.config.Iops = "6000"
	if _, err := p.CreateVolume("pvc-test", 20*gib); !errors.Is(err, caa.ErrVolumeAlreadyExists) {
		t.Fatalf("configuration mismatch: %v", err)
	}
}

func TestCreateFailureAndDuplicateName(t *testing.T) {
	p, s := testProvider(t)
	failure := errors.New("create API failure")
	s.createErr = failure
	if _, err := p.CreateVolume("pvc-test", 20*gib); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	s.createErr = nil
	if _, err := p.CreateVolume("pvc-test", 20*gib); err != nil {
		t.Fatal(err)
	}
	s.volumes = append(s.volumes, s.volumes[0])
	if _, err := p.CreateVolume("pvc-test", 20*gib); !errors.Is(err, caa.ErrVolumeAlreadyExists) {
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
	first, err := p.CreateVolume("pvc-test", 20*gib)
	if err != nil {
		t.Fatal(err)
	}
	// List conversion also omits Name, RG and encryption key.
	s.volumes[0].Name = nil
	s.pages = map[string]*provider.VolumeList{"": {Volumes: s.volumes}}
	second, err := p.CreateVolume("pvc-test", 20*gib)
	if err != nil || !reflect.DeepEqual(first, second) || s.creates != 1 {
		t.Fatalf("SDK-shaped retry: %+v %v", second, err)
	}
	s.volumes[0].Name = nil
	recovered, err := p.ListManagedVolumes()
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
	cfg, err := parseConfig(testParams())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Profile != "general-purpose" || cfg.FsType != "ext4" || cfg.Encrypted != "false" || cfg.BillingType != "hourly" {
		t.Fatalf("bad defaults: %+v", cfg)
	}
	aliases := map[string]string{"ibmRegion": "us-south", "ibmZone": "us-south-1", "ibmResourceGroup": "rg-test"}
	aliasConfig, err := parseConfig(aliases)
	if err != nil || !reflect.DeepEqual(cfg, aliasConfig) {
		t.Fatalf("alias parsing: %+v, %v", aliasConfig, err)
	}
	aliases["profile"] = "custom"
	aliases["ibmIops"] = "5000"
	aliases["tags"] = "first, second, "
	custom, err := parseConfig(aliases)
	if err != nil || custom.Iops != "5000" || len(custom.ExtraTags) != 2 {
		t.Fatalf("custom parsing: %+v, %v", custom, err)
	}
	for _, key := range []string{"region", "zone", "resourceGroup"} {
		t.Run("missing_"+key, func(t *testing.T) {
			params := testParams()
			delete(params, key)
			_, err := NewIBMCloudProvider(params)
			if !errors.Is(err, caa.ErrInvalidParameters) {
				t.Fatalf("got %v", err)
			}
		})
	}
	for _, change := range []map[string]string{
		{"zone": "us-east-1"}, {"profile": "unsupported"}, {"profile": "custom"},
		{"profile": "custom", "iops": "-1"}, {"iops": "5000"}, {"encrypted": "maybe"},
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
}

func TestCreateRetryAndDelete(t *testing.T) {
	p, s := testProvider(t)
	first, err := p.CreateVolume("pvc-test", 20*gib+1)
	if err != nil {
		t.Fatal(err)
	}
	if first.SizeBytes != 21*gib || first.Metadata["ibm-volume-id"] != first.Path {
		t.Fatalf("bad result: %+v", first)
	}
	restarted := &IBMCloudProvider{session: s, config: p.config}
	second, err := restarted.CreateVolume("pvc-test", 20*gib+1)
	if err != nil || !reflect.DeepEqual(first, second) || s.creates != 1 {
		t.Fatalf("retry: %+v %v creates=%d", second, err, s.creates)
	}
	if _, err := p.CreateVolume("pvc-test", 22*gib); !errors.Is(err, caa.ErrVolumeAlreadyExists) {
		t.Fatalf("larger retry: %v", err)
	}
	if err := p.DeleteVolume("pvc-test"); err != nil {
		t.Fatal(err)
	}
	if err := p.DeleteVolume("pvc-test"); err != nil || s.deletes != 1 {
		t.Fatalf("delete retry: %v count=%d", err, s.deletes)
	}
}

func TestLookupFailuresDoNotCreateOrDelete(t *testing.T) {
	for _, message := range []string{"forbidden", "timeout", "connection refused"} {
		t.Run(message, func(t *testing.T) {
			p, s := testProvider(t)
			failure := errors.New(message)
			s.listErr = failure
			if _, err := p.CreateVolume("pvc-test", 20*gib); !errors.Is(err, failure) {
				t.Fatal(err)
			}
			if _, err := p.VolumeExists("pvc-test"); !errors.Is(err, failure) {
				t.Fatal(err)
			}
			if err := p.DeleteVolume("pvc-test"); !errors.Is(err, failure) {
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
			if _, err := p.CreateVolume("pvc-test", 20*gib); err != nil {
				t.Fatal(err)
			}
			change(s.volumes[0])
			if _, err := p.CreateVolume("pvc-test", 20*gib); !errors.Is(err, caa.ErrVolumeAlreadyExists) {
				t.Fatalf("got %v", err)
			}
			if err := p.DeleteVolume("pvc-test"); !errors.Is(err, caa.ErrVolumeAlreadyExists) {
				t.Fatalf("delete: %v", err)
			}
			if s.creates != 1 || s.deletes != 0 {
				t.Fatal("mutated incompatible disk")
			}
		})
	}
}

func TestMalformedCreateResponses(t *testing.T) {
	for name, result := range map[string]func(provider.Volume) *provider.Volume{
		"nil":        func(provider.Volume) *provider.Volume { return nil },
		"missing-id": func(v provider.Volume) *provider.Volume { return &v },
		"pending":    func(v provider.Volume) *provider.Volume { v.VolumeID = "id"; v.Status = "pending"; return &v },
		"missing-capacity": func(v provider.Volume) *provider.Volume {
			v.VolumeID = "id"
			v.Status = "available"
			v.Capacity = nil
			return &v
		},
	} {
		t.Run(name, func(t *testing.T) {
			p, s := testProvider(t)
			s.createResult = result
			if _, err := p.CreateVolume("pvc-test", 20*gib); err == nil {
				t.Fatal("accepted incomplete result")
			}
		})
	}
}

func TestRecoveryOwnershipAndPagination(t *testing.T) {
	p, s := testProvider(t)
	if _, err := p.CreateVolume("pvc-test", 20*gib); err != nil {
		t.Fatal(err)
	}
	owned := s.volumes[0]
	foreign := *owned
	name := "unrelated-volume"
	foreign.Name = &name
	foreign.Tags = nil
	s.pages = map[string]*provider.VolumeList{"": {Volumes: []*provider.Volume{&foreign}, Next: "page2"}, "page2": {Volumes: []*provider.Volume{owned}}}
	vols, err := p.ListManagedVolumes()
	if err != nil || len(vols) != 1 || vols[0].VolumeID != "pvc-test" {
		t.Fatalf("recovery: %+v %v", vols, err)
	}
	s.pages["page2"].Next = "page2"
	if _, err := p.ListManagedVolumes(); err == nil {
		t.Fatal("accepted pagination loop")
	}
	s.pages = map[string]*provider.VolumeList{"": nil}
	if _, err := p.CreateVolume("pvc-new", 20*gib); err == nil {
		t.Fatal("nil list treated as absence")
	}
}
