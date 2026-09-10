// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package ibmcloud

import (
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

func (s *fakeSession) GetVolumeByName(name string) (*provider.Volume, error) {
	if s.lookupErr != nil {
		return nil, s.lookupErr
	}
	if volume, ok := s.volumes[name]; ok {
		return volume, nil
	}
	return nil, providerError.Message{Type: providerError.RetrivalFailed, RC: 404}
}

func (s *fakeSession) CreateVolume(volume provider.Volume) (*provider.Volume, error) {
	s.createCalls++
	s.lastCreate = volume
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
	return list, nil
}

func TestParseConfig(t *testing.T) {
	t.Run("defaults and aliases", func(t *testing.T) {
		cfg, err := parseConfig(map[string]string{
			"ibmRegion":        "us-south",
			"ibmZone":          "us-south-1",
			"ibmResourceGroup": "rg-alias",
			"tags":             "one, two",
		})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Region != "us-south" || cfg.Zone != "us-south-1" || cfg.ResourceGroup != "rg-alias" {
			t.Fatalf("aliases were not parsed: %+v", cfg)
		}
		if cfg.Profile != "general-purpose" {
			t.Fatalf("unexpected default profile: %q", cfg.Profile)
		}
		if len(cfg.ExtraTags) != 2 || cfg.ExtraTags[0] != "one" || cfg.ExtraTags[1] != "two" {
			t.Fatalf("unexpected tags: %v", cfg.ExtraTags)
		}
	})

	t.Run("SDP", func(t *testing.T) {
		cfg, err := parseConfig(map[string]string{
			"profile":    "sdp",
			"iops":       "3000",
			"throughput": "2000",
		})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Profile != "sdp" || cfg.Iops != "3000" || cfg.Throughput != 2000 {
			t.Fatalf("unexpected SDP config: %+v", cfg)
		}
	})

	for _, throughput := range []string{"invalid", "0", "-1"} {
		t.Run("invalid throughput "+throughput, func(t *testing.T) {
			if _, err := parseConfig(map[string]string{"throughput": throughput}); err == nil {
				t.Fatal("expected throughput parsing to fail")
			}
		})
	}
}

func TestResolveResourceGroup(t *testing.T) {
	if got := resolveResourceGroup("storage-class-rg", "provider-rg"); got != "storage-class-rg" {
		t.Fatalf("explicit resource group lost: %q", got)
	}
	if got := resolveResourceGroup("", "provider-rg"); got != "provider-rg" {
		t.Fatalf("provider resource group not used: %q", got)
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
		{
			name: "SDP",
			config: Config{
				Region: "us-south", Zone: "us-south-1", Profile: "sdp",
				ResourceGroup: "rg-provider", Iops: "3000", Throughput: 2000,
			},
			size:              1 * gib,
			wantSizeGB:        1,
			wantIOPS:          "3000",
			wantBandwidth:     2000,
			wantResourceGroup: "rg-provider",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := newFakeSession()
			ibmProvider := &IBMCloudProvider{session: session, config: test.config}

			info, err := ibmProvider.CreateVolume("pvc-test", test.size)
			if err != nil {
				t.Fatal(err)
			}
			request := session.lastCreate
			if session.createCalls != 1 || request.Capacity == nil || *request.Capacity != test.wantSizeGB {
				t.Fatalf("unexpected create capacity or count: %+v", request)
			}
			if request.Profile == nil || request.Profile.Name != test.config.Profile {
				t.Fatalf("unexpected profile: %+v", request.Profile)
			}
			if request.ResourceGroup == nil || request.ResourceGroup.ID != test.wantResourceGroup {
				t.Fatalf("unexpected resource group: %+v", request.ResourceGroup)
			}
			if request.Bandwidth != test.wantBandwidth {
				t.Fatalf("bandwidth=%d, want %d", request.Bandwidth, test.wantBandwidth)
			}
			if test.wantIOPS == "" {
				if request.Iops != nil {
					t.Fatalf("IOPS should not be sent for %s", test.config.Profile)
				}
			} else if request.Iops == nil || *request.Iops != test.wantIOPS {
				t.Fatalf("unexpected IOPS: %v", request.Iops)
			}
			if request.VolumeType != test.wantFilesystemType {
				t.Fatalf("filesystem=%q, want %q", request.VolumeType, test.wantFilesystemType)
			}
			if test.wantEncryptionKey != "" && (request.VolumeEncryptionKey == nil || request.VolumeEncryptionKey.CRN != test.wantEncryptionKey) {
				t.Fatalf("unexpected encryption key: %+v", request.VolumeEncryptionKey)
			}
			if len(request.Tags) == 0 || request.Tags[0] != "caa-csi-volume-id:pvc-test" {
				t.Fatalf("ownership tag missing: %v", request.Tags)
			}
			if info.VolumeID != "pvc-test" || info.Path != "r006-created-volume" {
				t.Fatalf("unexpected volume info: %+v", info)
			}
		})
	}
}

func TestCreateVolumeIdempotency(t *testing.T) {
	session := newFakeSession()
	name := "pvc-existing"
	capacity := 20
	session.volumes[name] = &provider.Volume{
		VolumeID: "r006-existing", Name: &name, Capacity: &capacity,
		VPCVolume: provider.VPCVolume{Profile: &provider.Profile{Name: "general-purpose"}},
	}
	ibmProvider := &IBMCloudProvider{session: session, config: Config{Profile: "general-purpose"}}

	info, err := ibmProvider.CreateVolume(name, 20*gib)
	if err != nil {
		t.Fatal(err)
	}
	if session.createCalls != 0 || info.Path != "r006-existing" {
		t.Fatalf("existing volume was not reused: %+v", info)
	}

	info, err = ibmProvider.CreateVolume(name, 1*gib)
	if err != nil {
		t.Fatalf("reusing existing volume: %v", err)
	}

	if info.Path != "r006-existing" {
		t.Fatalf("unexpected existing volume: %+v", info)
	}
	if session.createCalls != 0 {
		t.Fatal("capacity conflict must not create another volume")
	}
}

func TestCreateVolumePropagatesLookupFailure(t *testing.T) {
	session := newFakeSession()
	session.lookupErr = providerError.Message{Type: providerError.PermissionDenied, RC: 403, Description: "forbidden"}
	ibmProvider := &IBMCloudProvider{session: session, config: Config{Profile: "general-purpose"}}

	_, err := ibmProvider.CreateVolume("pvc-denied", 20*gib)
	if err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("lookup error was hidden: %v", err)
	}
	if session.createCalls != 0 {
		t.Fatal("create must not run after a lookup failure")
	}
}

func TestDeleteVolume(t *testing.T) {
	t.Run("existing", func(t *testing.T) {
		session := newFakeSession()
		name := "pvc-delete"
		session.volumes[name] = &provider.Volume{VolumeID: "r006-delete", Name: &name}
		ibmProvider := &IBMCloudProvider{session: session}

		if err := ibmProvider.DeleteVolume(name); err != nil {
			t.Fatal(err)
		}
		if session.deleteCalls != 1 || session.lastDelete != "r006-delete" {
			t.Fatalf("native IBM ID was not deleted: calls=%d id=%q", session.deleteCalls, session.lastDelete)
		}
	})

	t.Run("already absent", func(t *testing.T) {
		session := newFakeSession()
		ibmProvider := &IBMCloudProvider{session: session}

		if err := ibmProvider.DeleteVolume("pvc-absent"); err != nil {
			t.Fatal(err)
		}
		if session.deleteCalls != 0 {
			t.Fatal("delete should not be sent for an absent volume")
		}
	})

	t.Run("deleted between lookup and delete", func(t *testing.T) {
		session := newFakeSession()
		name := "pvc-delete-race"
		session.volumes[name] = &provider.Volume{VolumeID: "r006-delete-race", Name: &name}
		session.deleteErr = providerError.Message{Type: providerError.EntityNotFound, RC: 404}
		ibmProvider := &IBMCloudProvider{session: session}

		if err := ibmProvider.DeleteVolume(name); err != nil {
			t.Fatalf("already-deleted volume should succeed: %v", err)
		}
		if session.deleteCalls != 1 {
			t.Fatalf("delete calls=%d, want 1", session.deleteCalls)
		}
	})

	t.Run("lookup failure", func(t *testing.T) {
		session := newFakeSession()
		session.lookupErr = providerError.Message{Type: providerError.PermissionDenied, RC: 403, Description: "forbidden"}
		ibmProvider := &IBMCloudProvider{session: session}

		err := ibmProvider.DeleteVolume("pvc-denied")
		if err == nil || !strings.Contains(err.Error(), "forbidden") {
			t.Fatalf("lookup error was hidden: %v", err)
		}
		if session.deleteCalls != 0 {
			t.Fatal("delete must not run after a lookup failure")
		}
	})
}

func TestVolumeExistsPropagatesLookupFailure(t *testing.T) {
	session := newFakeSession()
	session.lookupErr = providerError.Message{Type: providerError.Unauthenticated, RC: 401, Description: "unauthenticated"}
	ibmProvider := &IBMCloudProvider{session: session}

	if exists, err := ibmProvider.VolumeExists("pvc-test"); err == nil || exists {
		t.Fatalf("lookup failure was hidden: exists=%v err=%v", exists, err)
	}
}
