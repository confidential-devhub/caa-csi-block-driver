// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package ibmcloud

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/IBM/ibmcloud-volume-interface/lib/provider"
	providerError "github.com/IBM/ibmcloud-volume-interface/lib/utils"

	caa "github.com/confidential-devhub/caa-csi-block-driver/pkg/provider"
)

// Snapshot behaviour of the shared fakeSession (see provider_test.go).
type fakeSnapshots struct {
	snapshots       []*provider.Snapshot
	snapshotCreates int
	snapshotDeletes int
	notReadyPolls   int // GetSnapshot reports not-ready this many times
	snapListErr     error
}

func snaps(s *fakeSession) *fakeSnapshots { return &s.fakeSnapshots }

func (s *fakeSession) CreateSnapshot(sourceVolumeID string, params provider.SnapshotParameters) (*provider.Snapshot, error) {
	st := snaps(s)
	st.snapshotCreates++
	snap := &provider.Snapshot{
		VolumeID:             sourceVolumeID,
		SnapshotID:           fmt.Sprintf("r006-snap-%d", st.snapshotCreates),
		SnapshotSize:         20 * gib,
		SnapshotCreationTime: time.Unix(1700000000, 0),
		ReadyToUse:           false,
		VPC:                  provider.VPC{Name: params.Name},
	}
	st.snapshots = append(st.snapshots, snap)
	copied := *snap
	return &copied, nil
}

func (s *fakeSession) DeleteSnapshot(snap *provider.Snapshot) error {
	st := snaps(s)
	st.snapshotDeletes++
	for i, existing := range st.snapshots {
		if existing.SnapshotID == snap.SnapshotID {
			st.snapshots = append(st.snapshots[:i], st.snapshots[i+1:]...)
			return nil
		}
	}
	return providerError.Message{Code: "SnapshotIDNotFound", RC: 404, BackendError: "Code:snapshot_id_not_found"}
}

func (s *fakeSession) GetSnapshot(id string, _ ...string) (*provider.Snapshot, error) {
	st := snaps(s)
	for _, existing := range st.snapshots {
		if existing.SnapshotID == id {
			copied := *existing
			if st.notReadyPolls > 0 {
				st.notReadyPolls--
				copied.ReadyToUse = false
			} else {
				copied.ReadyToUse = true
			}
			return &copied, nil
		}
	}
	return nil, providerError.Message{Code: "SnapshotIDNotFound", RC: 404, BackendError: "Code:snapshot_id_not_found"}
}

func (s *fakeSession) ListSnapshots(_ int, _ string, filters map[string]string) (*provider.SnapshotList, error) {
	st := snaps(s)
	if st.snapListErr != nil {
		return nil, st.snapListErr
	}
	list := &provider.SnapshotList{}
	for _, snap := range st.snapshots {
		if filters["name"] != "" && snap.Name != filters["name"] {
			continue
		}
		if filters["source_volume.id"] != "" && snap.VolumeID != filters["source_volume.id"] {
			continue
		}
		list.Snapshots = append(list.Snapshots, snap)
	}
	return list, nil
}

func markReady(s *fakeSession) {
	for _, snap := range snaps(s).snapshots {
		snap.ReadyToUse = true
	}
}

func snapshotTestProvider(t *testing.T) (*IBMCloudProvider, *fakeSession) {
	t.Helper()
	snapshotPollInterval = time.Millisecond
	p, s := testProvider(t)
	if _, err := p.CreateVolume(context.TODO(), "pvc-src", 20*gib); err != nil {
		t.Fatal(err)
	}
	return p, s
}

func TestSnapshotCreateIsIdempotent(t *testing.T) {
	p, s := snapshotTestProvider(t)
	first, err := p.CreateSnapshot(context.TODO(), "pvc-src", "snap-1")
	if err != nil {
		t.Fatal(err)
	}
	if first.SnapshotID != "r006-snap-1" || first.SourceVolumeID != "pvc-src" || first.SizeBytes != 20*gib ||
		first.CreationTime != 1700000000 || first.ReadyToUse {
		t.Fatalf("unexpected snapshot: %+v", first)
	}
	second, err := p.CreateSnapshot(context.TODO(), "pvc-src", "snap-1")
	if err != nil || second.SnapshotID != first.SnapshotID || snaps(s).snapshotCreates != 1 {
		t.Fatalf("retry: %+v %v creates=%d", second, err, snaps(s).snapshotCreates)
	}
}

func TestSnapshotCreateRejectsBadInput(t *testing.T) {
	p, s := snapshotTestProvider(t)
	if _, err := p.CreateSnapshot(context.TODO(), "pvc-src", ""); !errors.Is(err, caa.ErrInvalidParameters) {
		t.Fatalf("empty name: %v", err)
	}
	if _, err := p.CreateSnapshot(context.TODO(), "pvc-missing", "snap-1"); !errors.Is(err, caa.ErrVolumeNotFound) {
		t.Fatalf("missing source: %v", err)
	}
	if _, err := p.CreateSnapshot(context.TODO(), "pvc-src", "snap-1"); err != nil {
		t.Fatal(err)
	}
	// Same snapshot name for a different source volume is a conflict.
	if _, err := p.CreateVolume(context.TODO(), "pvc-other", 20*gib); err != nil {
		t.Fatal(err)
	}
	s.volumes[1].VolumeID = "r006-other"
	if _, err := p.CreateSnapshot(context.TODO(), "pvc-other", "snap-1"); !errors.Is(err, caa.ErrVolumeAlreadyExists) {
		t.Fatalf("different source: %v", err)
	}
}

func TestSnapshotFindAndList(t *testing.T) {
	p, s := snapshotTestProvider(t)
	if _, err := p.CreateSnapshot(context.TODO(), "pvc-src", "snap-1"); err != nil {
		t.Fatal(err)
	}
	markReady(s)

	for _, ref := range []string{"snap-1", "r006-snap-1"} {
		found, err := p.FindSnapshot(context.TODO(), ref)
		if err != nil || found == nil || found.SnapshotID != "r006-snap-1" || found.SourceVolumeID != "pvc-src" || !found.ReadyToUse {
			t.Fatalf("FindSnapshot(%s) = %+v, %v", ref, found, err)
		}
	}
	if found, err := p.FindSnapshot(context.TODO(), "absent"); err != nil || found != nil {
		t.Fatalf("absent snapshot: %+v %v", found, err)
	}

	for _, volumeID := range []string{"pvc-src", ""} {
		list, err := p.ListSnapshots(context.TODO(), volumeID)
		if err != nil || len(list) != 1 || list[0].SourceVolumeID != "pvc-src" {
			t.Fatalf("ListSnapshots(%q) = %+v, %v", volumeID, list, err)
		}
	}
	if list, err := p.ListSnapshots(context.TODO(), "pvc-missing"); err != nil || len(list) != 0 {
		t.Fatalf("missing volume list: %+v %v", list, err)
	}

	// Snapshots of volumes not owned by this driver are not reported.
	s.volumes[0].Tags = nil
	if list, err := p.ListSnapshots(context.TODO(), ""); err != nil || len(list) != 0 {
		t.Fatalf("foreign snapshot listed: %+v %v", list, err)
	}
}

func TestSnapshotLookupFailureIsNotAbsence(t *testing.T) {
	p, s := snapshotTestProvider(t)
	failure := errors.New("forbidden")
	snaps(s).snapListErr = failure
	if _, err := p.FindSnapshot(context.TODO(), "snap-1"); !errors.Is(err, failure) {
		t.Fatalf("find: %v", err)
	}
	if err := p.DeleteSnapshot(context.TODO(), "snap-1"); !errors.Is(err, failure) {
		t.Fatalf("delete: %v", err)
	}
	if snaps(s).snapshotDeletes != 0 {
		t.Fatal("deleted after lookup failure")
	}
}

func TestSnapshotDelete(t *testing.T) {
	p, s := snapshotTestProvider(t)
	if _, err := p.CreateSnapshot(context.TODO(), "pvc-src", "snap-1"); err != nil {
		t.Fatal(err)
	}
	if err := p.DeleteSnapshot(context.TODO(), "snap-1"); err != nil {
		t.Fatal(err)
	}
	if err := p.DeleteSnapshot(context.TODO(), "snap-1"); err != nil || snaps(s).snapshotDeletes != 1 {
		t.Fatalf("delete retry: %v count=%d", err, snaps(s).snapshotDeletes)
	}
}

func TestCreateVolumeFromSnapshot(t *testing.T) {
	p, s := snapshotTestProvider(t)
	if _, err := p.CreateSnapshot(context.TODO(), "pvc-src", "snap-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateVolumeFromSnapshot(context.TODO(), "pvc-restored", "snap-1", 20*gib); err == nil {
		t.Fatal("restored from a snapshot that is not ready")
	}
	markReady(s)
	if _, err := p.CreateVolumeFromSnapshot(context.TODO(), "pvc-restored", "absent", 20*gib); err == nil {
		t.Fatal("restored from a missing snapshot")
	}
	if _, err := p.CreateVolumeFromSnapshot(context.TODO(), "pvc-small", "snap-1", 10*gib); !errors.Is(err, caa.ErrInvalidParameters) {
		t.Fatalf("smaller than snapshot: %v", err)
	}

	info, err := p.CreateVolumeFromSnapshot(context.TODO(), "pvc-restored", "snap-1", 20*gib)
	if err != nil {
		t.Fatal(err)
	}
	restored := s.volumes[len(s.volumes)-1]
	if restored.SnapshotID != "r006-snap-1" || info.Metadata["source-snapshot"] != "r006-snap-1" || info.VolumeID != "pvc-restored" {
		t.Fatalf("snapshot not used: request=%+v info=%+v", restored, info)
	}
	creates := s.creates
	again, err := p.CreateVolumeFromSnapshot(context.TODO(), "pvc-restored", "snap-1", 20*gib)
	if err != nil || again.Path != info.Path || s.creates != creates {
		t.Fatalf("retry: %+v %v", again, err)
	}
}

func TestCreateVolumeFromVolume(t *testing.T) {
	p, s := snapshotTestProvider(t)
	snaps(s).notReadyPolls = 2

	info, err := p.CreateVolumeFromVolume(context.TODO(), "pvc-clone", "pvc-src", 20*gib)
	if err != nil {
		t.Fatal(err)
	}
	clone := s.volumes[len(s.volumes)-1]
	if clone.SnapshotID == "" || info.VolumeID != "pvc-clone" {
		t.Fatalf("clone not seeded from snapshot: %+v %+v", clone, info)
	}
	if st := snaps(s); st.snapshotCreates != 1 || st.snapshotDeletes != 1 || len(st.snapshots) != 0 {
		t.Fatalf("temporary snapshot not cleaned up: %+v", st)
	}
	creates := s.creates
	if _, err := p.CreateVolumeFromVolume(context.TODO(), "pvc-clone", "pvc-src", 20*gib); err != nil ||
		s.creates != creates || snaps(s).snapshotCreates != 1 {
		t.Fatalf("clone retry must not repeat work: %v", err)
	}
}

func TestCreateVolumeFromVolumeCleansUpOnFailure(t *testing.T) {
	p, s := snapshotTestProvider(t)
	s.createErr = errors.New("quota exceeded")
	if _, err := p.CreateVolumeFromVolume(context.TODO(), "pvc-clone", "pvc-src", 20*gib); !errors.Is(err, s.createErr) {
		t.Fatalf("want create error, got %v", err)
	}
	if st := snaps(s); len(st.snapshots) != 0 || st.snapshotDeletes != 1 {
		t.Fatalf("temporary snapshot leaked: %+v", st)
	}
	if _, err := p.CreateVolumeFromVolume(context.TODO(), "pvc-clone", "pvc-missing", 20*gib); !errors.Is(err, caa.ErrVolumeNotFound) {
		t.Fatalf("missing source: %v", err)
	}
}

const srcNativeID = "r006-test-volume" // native ID the fake assigns to every created volume

// snapSession wraps fakeSession so a test can inject pagination, SDK failures
// and per-volume lookup errors. Anything left unset delegates to the fake.
type snapSession struct {
	*fakeSession
	snapPages       map[string]*provider.SnapshotList
	lastSnapFilters map[string]string
	createSnap      func(string, provider.SnapshotParameters) (*provider.Snapshot, error)
	deleteErr       error
	getSnapErr      error
	getVolumeErr    map[string]error
	getVolumeCalls  int
}

func wrapSession(p *IBMCloudProvider, s *fakeSession) *snapSession {
	w := &snapSession{fakeSession: s}
	p.session = w
	return w
}

func (w *snapSession) ListSnapshots(limit int, start string, filters map[string]string) (*provider.SnapshotList, error) {
	w.lastSnapFilters = filters
	if w.snapPages != nil {
		return w.snapPages[start], nil
	}
	return w.fakeSession.ListSnapshots(limit, start, filters)
}

func (w *snapSession) CreateSnapshot(id string, params provider.SnapshotParameters) (*provider.Snapshot, error) {
	if w.createSnap != nil {
		return w.createSnap(id, params)
	}
	return w.fakeSession.CreateSnapshot(id, params)
}

func (w *snapSession) DeleteSnapshot(snap *provider.Snapshot) error {
	if w.deleteErr != nil {
		return w.deleteErr
	}
	return w.fakeSession.DeleteSnapshot(snap)
}

func (w *snapSession) GetSnapshot(id string, extra ...string) (*provider.Snapshot, error) {
	if w.getSnapErr != nil {
		return nil, w.getSnapErr
	}
	return w.fakeSession.GetSnapshot(id, extra...)
}

func (w *snapSession) GetVolume(id string) (*provider.Volume, error) {
	w.getVolumeCalls++
	if err := w.getVolumeErr[id]; err != nil {
		return nil, err
	}
	return w.fakeSession.GetVolume(id)
}

func notFoundErr(backend string) error {
	return providerError.Message{Code: "SnapshotIDNotFound", RC: 404, BackendError: backend}
}

// addSnapshot seeds a named snapshot of volumeNativeID directly in the fake backend.
func addSnapshot(s *fakeSession, name, volumeNativeID string) {
	_, _ = s.CreateSnapshot(volumeNativeID, provider.SnapshotParameters{Name: name})
}

func TestListSnapshotsPaginationAndFilters(t *testing.T) {
	p, s := snapshotTestProvider(t)
	w := wrapSession(p, s)
	w.snapPages = map[string]*provider.SnapshotList{
		"":   {Snapshots: []*provider.Snapshot{{SnapshotID: "r006-a", VolumeID: srcNativeID}, nil}, Next: "p2"},
		"p2": {Snapshots: []*provider.Snapshot{{SnapshotID: "r006-b", VolumeID: srcNativeID}}},
	}

	list, err := p.ListSnapshots(context.TODO(), "")
	if err != nil || len(list) != 2 || list[0].SnapshotID != "r006-a" || list[1].SnapshotID != "r006-b" {
		t.Fatalf("pagination: %+v %v", list, err)
	}
	if w.lastSnapFilters["resource_group.id"] != "rg-test" || w.lastSnapFilters["source_volume.id"] != "" {
		t.Fatalf("unscoped list filters: %v", w.lastSnapFilters)
	}

	if _, err := p.ListSnapshots(context.TODO(), "pvc-src"); err != nil {
		t.Fatal(err)
	}
	if w.lastSnapFilters["resource_group.id"] != "rg-test" || w.lastSnapFilters["source_volume.id"] != srcNativeID {
		t.Fatalf("volume-scoped list filters: %v", w.lastSnapFilters)
	}

	w.snapPages["p2"].Next = "p2"
	if _, err := p.ListSnapshots(context.TODO(), ""); err == nil {
		t.Fatal("accepted a pagination loop")
	}
	w.snapPages = map[string]*provider.SnapshotList{"": nil}
	if _, err := p.ListSnapshots(context.TODO(), ""); err == nil {
		t.Fatal("nil list treated as empty")
	}
}

func TestSnapshotAmbiguousNameIsRejected(t *testing.T) {
	p, s := snapshotTestProvider(t)
	addSnapshot(s, "dup", srcNativeID)
	addSnapshot(s, "dup", srcNativeID)
	markReady(s)
	created := s.snapshotCreates

	if _, err := p.FindSnapshot(context.TODO(), "dup"); !errors.Is(err, caa.ErrVolumeAlreadyExists) {
		t.Fatalf("find: %v", err)
	}
	if err := p.DeleteSnapshot(context.TODO(), "dup"); !errors.Is(err, caa.ErrVolumeAlreadyExists) || s.snapshotDeletes != 0 {
		t.Fatalf("delete must not pick one arbitrarily: %v deletes=%d", err, s.snapshotDeletes)
	}
	if _, err := p.CreateSnapshot(context.TODO(), "pvc-src", "dup"); !errors.Is(err, caa.ErrVolumeAlreadyExists) || s.snapshotCreates != created {
		t.Fatalf("create: %v", err)
	}
	if _, err := p.CreateVolumeFromSnapshot(context.TODO(), "pvc-r", "dup", 20*gib); !errors.Is(err, caa.ErrVolumeAlreadyExists) {
		t.Fatalf("restore: %v", err)
	}
}

func TestSnapshotNativeIDLookupErrors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		wantAbsent bool
	}{
		{"genuine not found", notFoundErr("Code:snapshot_id_not_found"), true},
		{"transport failure", errors.New("connection refused"), false},
		// The SDK maps every GetSnapshot failure to a 404 "ID not found"
		// message, so a 404 whose backend error is not a not-found must fail.
		{"masked failure", notFoundErr("Code:internal_error, Description:timeout"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, s := snapshotTestProvider(t)
			wrapSession(p, s).getSnapErr = tc.err

			found, err := p.FindSnapshot(context.TODO(), "r006-unknown")
			delErr := p.DeleteSnapshot(context.TODO(), "r006-unknown")
			if tc.wantAbsent {
				if err != nil || found != nil || delErr != nil {
					t.Fatalf("absence: %+v %v %v", found, err, delErr)
				}
				return
			}
			if err == nil || delErr == nil || s.snapshotDeletes != 0 {
				t.Fatalf("failure treated as absence: %v %v deletes=%d", err, delErr, s.snapshotDeletes)
			}
		})
	}
}

func TestSnapshotSourceVolumeResolution(t *testing.T) {
	setup := func(t *testing.T) (*IBMCloudProvider, *fakeSession, *snapSession) {
		p, s := snapshotTestProvider(t)
		addSnapshot(s, "snap-a", srcNativeID)
		addSnapshot(s, "snap-b", srcNativeID)
		markReady(s)
		return p, s, wrapSession(p, s)
	}

	t.Run("lookup is cached per volume", func(t *testing.T) {
		p, _, w := setup(t)
		list, err := p.ListSnapshots(context.TODO(), "")
		if err != nil || len(list) != 2 || w.getVolumeCalls != 1 {
			t.Fatalf("list=%d err=%v GetVolume calls=%d", len(list), err, w.getVolumeCalls)
		}
	})

	t.Run("deleted source volume is unattributed", func(t *testing.T) {
		p, _, w := setup(t)
		w.getVolumeErr = map[string]error{srcNativeID: notFoundErr("Code:volume_id_not_found")}
		if list, err := p.ListSnapshots(context.TODO(), ""); err != nil || len(list) != 0 {
			t.Fatalf("list: %+v %v", list, err)
		}
		found, err := p.FindSnapshot(context.TODO(), "snap-a")
		if err != nil || found == nil || found.SourceVolumeID != "" {
			t.Fatalf("find: %+v %v", found, err)
		}
	})

	t.Run("source lookup failure is an error", func(t *testing.T) {
		p, _, w := setup(t)
		failure := errors.New("denied")
		w.getVolumeErr = map[string]error{srcNativeID: failure}
		if _, err := p.ListSnapshots(context.TODO(), ""); !errors.Is(err, failure) {
			t.Fatalf("list: %v", err)
		}
		if _, err := p.FindSnapshot(context.TODO(), "snap-a"); !errors.Is(err, failure) {
			t.Fatalf("find: %v", err)
		}
	})

	t.Run("conflicting ownership tags are unattributed", func(t *testing.T) {
		p, s, _ := setup(t)
		s.volumes[0].Tags = append(s.volumes[0].Tags, ownershipTagPrefix+"someone-else")
		if list, err := p.ListSnapshots(context.TODO(), ""); err != nil || len(list) != 0 {
			t.Fatalf("list: %+v %v", list, err)
		}
	})

	t.Run("snapshot without source volume is skipped", func(t *testing.T) {
		p, s, _ := setup(t)
		s.snapshots[0].VolumeID, s.snapshots[1].VolumeID = "", ""
		if list, err := p.ListSnapshots(context.TODO(), ""); err != nil || len(list) != 0 {
			t.Fatalf("list: %+v %v", list, err)
		}
	})
}

func TestCreateSnapshotFailures(t *testing.T) {
	t.Run("source volume not owned by driver", func(t *testing.T) {
		p, s := snapshotTestProvider(t)
		s.volumes[0].Tags = nil
		if _, err := p.CreateSnapshot(context.TODO(), "pvc-src", "snap-1"); !errors.Is(err, caa.ErrVolumeAlreadyExists) || s.snapshotCreates != 0 {
			t.Fatalf("snapshotted a foreign volume: %v creates=%d", err, s.snapshotCreates)
		}
	})

	t.Run("sdk error", func(t *testing.T) {
		p, s := snapshotTestProvider(t)
		failure := errors.New("quota exceeded")
		wrapSession(p, s).createSnap = func(string, provider.SnapshotParameters) (*provider.Snapshot, error) { return nil, failure }
		if _, err := p.CreateSnapshot(context.TODO(), "pvc-src", "snap-1"); !errors.Is(err, failure) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("sdk request carries name, source and resource group", func(t *testing.T) {
		p, s := snapshotTestProvider(t)
		var gotID string
		var gotParams provider.SnapshotParameters
		wrapSession(p, s).createSnap = func(id string, params provider.SnapshotParameters) (*provider.Snapshot, error) {
			gotID, gotParams = id, params
			return &provider.Snapshot{SnapshotID: "r006-x", VolumeID: id}, nil
		}
		info, err := p.CreateSnapshot(context.TODO(), "pvc-src", "snap-1")
		if err != nil || gotID != srcNativeID || gotParams.Name != "snap-1" || gotParams.ResourceGroup != "rg-test" {
			t.Fatalf("request: id=%q params=%+v err=%v", gotID, gotParams, err)
		}
		if info.CreationTime != 0 || info.ReadyToUse {
			t.Fatalf("zero creation time must map to 0: %+v", info)
		}
	})

	t.Run("incomplete sdk response", func(t *testing.T) {
		for name, snap := range map[string]*provider.Snapshot{"nil": nil, "no id": {}} {
			t.Run(name, func(t *testing.T) {
				p, s := snapshotTestProvider(t)
				wrapSession(p, s).createSnap = func(string, provider.SnapshotParameters) (*provider.Snapshot, error) { return snap, nil }
				if _, err := p.CreateSnapshot(context.TODO(), "pvc-src", "snap-1"); err == nil {
					t.Fatal("accepted incomplete response")
				}
			})
		}
	})
}

func TestDeleteSnapshotSDKErrors(t *testing.T) {
	p, s := snapshotTestProvider(t)
	if _, err := p.CreateSnapshot(context.TODO(), "pvc-src", "snap-1"); err != nil {
		t.Fatal(err)
	}
	w := wrapSession(p, s)

	busy := errors.New("snapshot busy")
	w.deleteErr = busy
	if err := p.DeleteSnapshot(context.TODO(), "snap-1"); !errors.Is(err, busy) {
		t.Fatalf("delete failure swallowed: %v", err)
	}
	w.deleteErr = notFoundErr("Code:snapshot_id_not_found") // deleted concurrently
	if err := p.DeleteSnapshot(context.TODO(), "snap-1"); err != nil {
		t.Fatalf("concurrent delete must be idempotent: %v", err)
	}
	if err := p.DeleteSnapshot(context.TODO(), ""); !errors.Is(err, caa.ErrInvalidParameters) {
		t.Fatalf("empty ID: %v", err)
	}
}

func TestCreateVolumeFromSnapshotVariants(t *testing.T) {
	t.Run("by native snapshot ID", func(t *testing.T) {
		p, s := snapshotTestProvider(t)
		if _, err := p.CreateSnapshot(context.TODO(), "pvc-src", "snap-1"); err != nil {
			t.Fatal(err)
		}
		markReady(s)
		info, err := p.CreateVolumeFromSnapshot(context.TODO(), "pvc-r", "r006-snap-1", 20*gib)
		if err != nil || s.volumes[len(s.volumes)-1].SnapshotID != "r006-snap-1" || info.Metadata["source-snapshot"] != "r006-snap-1" {
			t.Fatalf("restore: %+v %v", info, err)
		}
	})

	t.Run("larger volume than snapshot is allowed", func(t *testing.T) {
		p, s := snapshotTestProvider(t)
		if _, err := p.CreateSnapshot(context.TODO(), "pvc-src", "snap-1"); err != nil {
			t.Fatal(err)
		}
		markReady(s)
		info, err := p.CreateVolumeFromSnapshot(context.TODO(), "pvc-r", "snap-1", 30*gib)
		if err != nil || info.SizeBytes != 30*gib {
			t.Fatalf("restore: %+v %v", info, err)
		}
	})

	t.Run("lookup failure creates nothing", func(t *testing.T) {
		p, s := snapshotTestProvider(t)
		failure := errors.New("forbidden")
		s.snapListErr = failure
		creates := s.creates
		if _, err := p.CreateVolumeFromSnapshot(context.TODO(), "pvc-r", "snap-1", 20*gib); !errors.Is(err, failure) || s.creates != creates {
			t.Fatalf("got %v creates=%d->%d", err, creates, s.creates)
		}
	})

	t.Run("empty snapshot or volume name", func(t *testing.T) {
		p, _ := snapshotTestProvider(t)
		if _, err := p.CreateVolumeFromVolume(context.TODO(), "", "pvc-src", 20*gib); !errors.Is(err, caa.ErrInvalidParameters) {
			t.Fatalf("empty clone name: %v", err)
		}
		if _, err := p.FindSnapshot(context.TODO(), ""); !errors.Is(err, caa.ErrInvalidParameters) {
			t.Fatalf("empty snapshot ID: %v", err)
		}
	})
}

func TestCreateVolumeFromVolumeWaitFailures(t *testing.T) {
	t.Run("context cancelled while snapshot is pending", func(t *testing.T) {
		p, s := snapshotTestProvider(t)
		s.notReadyPolls = 1000
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		creates := s.creates
		if _, err := p.CreateVolumeFromVolume(ctx, "pvc-clone", "pvc-src", 20*gib); !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
		if s.creates != creates || len(s.snapshots) != 0 {
			t.Fatalf("volume created or temp snapshot leaked: creates=%d snapshots=%d", s.creates, len(s.snapshots))
		}
	})

	t.Run("snapshot status lookup fails", func(t *testing.T) {
		p, s := snapshotTestProvider(t)
		failure := errors.New("api down")
		wrapSession(p, s).getSnapErr = failure
		creates := s.creates
		if _, err := p.CreateVolumeFromVolume(context.TODO(), "pvc-clone", "pvc-src", 20*gib); !errors.Is(err, failure) || s.creates != creates {
			t.Fatalf("got %v creates=%d->%d", err, creates, s.creates)
		}
	})

	t.Run("polls until the snapshot becomes ready", func(t *testing.T) {
		p, s := snapshotTestProvider(t)
		s.notReadyPolls = 3
		if _, err := p.CreateVolumeFromVolume(context.TODO(), "pvc-clone", "pvc-src", 20*gib); err != nil {
			t.Fatal(err)
		}
		if s.notReadyPolls != 0 {
			t.Fatalf("returned before the snapshot was ready: %d polls left", s.notReadyPolls)
		}
	})
}
