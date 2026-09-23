// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func TestNewAWSProvider_Validation(t *testing.T) {
	tests := []struct {
		name      string
		params    map[string]string
		wantErr   bool
		errSubstr string
	}{
		{
			name:      "missing region",
			params:    map[string]string{},
			wantErr:   true,
			errSubstr: "awsRegion is required",
		},
		{
			name:    "region only is valid",
			params:  map[string]string{"awsRegion": "us-east-1"},
			wantErr: false,
		},
		{
			name:      "invalid IOPS - not a number",
			params:    map[string]string{"awsRegion": "us-east-1", "awsIops": "abc"},
			wantErr:   true,
			errSubstr: "invalid awsIops",
		},
		{
			name:      "invalid IOPS - zero",
			params:    map[string]string{"awsRegion": "us-east-1", "awsIops": "0"},
			wantErr:   true,
			errSubstr: "invalid awsIops",
		},
		{
			name:      "invalid IOPS - negative",
			params:    map[string]string{"awsRegion": "us-east-1", "awsIops": "-100"},
			wantErr:   true,
			errSubstr: "invalid awsIops",
		},
		{
			name:      "invalid throughput - not a number",
			params:    map[string]string{"awsRegion": "us-east-1", "awsThroughput": "fast"},
			wantErr:   true,
			errSubstr: "invalid awsThroughput",
		},
		{
			name:      "invalid throughput - zero",
			params:    map[string]string{"awsRegion": "us-east-1", "awsThroughput": "0"},
			wantErr:   true,
			errSubstr: "invalid awsThroughput",
		},
		{
			name: "IOPS rejected for gp2",
			params: map[string]string{
				"awsRegion":     "us-east-1",
				"awsVolumeType": "gp2",
				"awsIops":       "3000",
			},
			wantErr:   true,
			errSubstr: "awsIops is only supported for",
		},
		{
			name: "IOPS rejected for st1",
			params: map[string]string{
				"awsRegion":     "us-east-1",
				"awsVolumeType": "st1",
				"awsIops":       "3000",
			},
			wantErr:   true,
			errSubstr: "awsIops is only supported for",
		},
		{
			name: "throughput rejected for io1",
			params: map[string]string{
				"awsRegion":     "us-east-1",
				"awsVolumeType": "io1",
				"awsThroughput": "125",
			},
			wantErr:   true,
			errSubstr: "awsThroughput is only supported for gp3",
		},
		{
			name: "IOPS accepted for gp3",
			params: map[string]string{
				"awsRegion":     "us-east-1",
				"awsVolumeType": "gp3",
				"awsIops":       "3000",
			},
			wantErr: false,
		},
		{
			name: "IOPS accepted for io1",
			params: map[string]string{
				"awsRegion":     "us-east-1",
				"awsVolumeType": "io1",
				"awsIops":       "5000",
			},
			wantErr: false,
		},
		{
			name: "IOPS accepted for io2",
			params: map[string]string{
				"awsRegion":     "us-east-1",
				"awsVolumeType": "io2",
				"awsIops":       "5000",
			},
			wantErr: false,
		},
		{
			name: "throughput accepted for gp3",
			params: map[string]string{
				"awsRegion":     "us-east-1",
				"awsVolumeType": "gp3",
				"awsThroughput": "250",
			},
			wantErr: false,
		},
		{
			name: "default volume type is gp3",
			params: map[string]string{
				"awsRegion": "us-east-1",
				"awsIops":   "3000",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewAWSProvider(tt.params)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error but got nil")
				}
				if tt.errSubstr != "" && !strings.Contains(err.Error(), tt.errSubstr) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.errSubstr)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestSafeUnix(t *testing.T) {
	t.Run("nil time returns zero", func(t *testing.T) {
		if got := safeUnix(nil); got != 0 {
			t.Errorf("safeUnix(nil) = %d, want 0", got)
		}
	})

	t.Run("valid time returns unix seconds", func(t *testing.T) {
		ts := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
		got := safeUnix(&ts)
		if got != ts.Unix() {
			t.Errorf("safeUnix(%v) = %d, want %d", ts, got, ts.Unix())
		}
	})
}

func TestEbsSnapshotToInfo(t *testing.T) {
	p := &AWSProvider{}

	t.Run("extracts tags and size", func(t *testing.T) {
		snap := &ec2types.Snapshot{
			SnapshotId: aws.String("snap-abc123"),
			VolumeSize: aws.Int32(10),
			State:      ec2types.SnapshotStateCompleted,
			Tags: []ec2types.Tag{
				{Key: aws.String("caa-csi-snapshot-id"), Value: aws.String("my-snap")},
				{Key: aws.String(volumeTagKey), Value: aws.String("my-vol")},
			},
		}
		info := p.ebsSnapshotToInfo(snap)
		if info.SnapshotID != "my-snap" {
			t.Errorf("SnapshotID = %q, want %q", info.SnapshotID, "my-snap")
		}
		if info.SourceVolumeID != "my-vol" {
			t.Errorf("SourceVolumeID = %q, want %q", info.SourceVolumeID, "my-vol")
		}
		if info.SizeBytes != 10*1024*1024*1024 {
			t.Errorf("SizeBytes = %d, want %d", info.SizeBytes, 10*1024*1024*1024)
		}
		if !info.ReadyToUse {
			t.Error("ReadyToUse should be true for completed snapshot")
		}
	})

	t.Run("pending snapshot is not ready", func(t *testing.T) {
		snap := &ec2types.Snapshot{
			SnapshotId: aws.String("snap-pending"),
			VolumeSize: aws.Int32(5),
			State:      ec2types.SnapshotStatePending,
			Tags:       []ec2types.Tag{},
		}
		info := p.ebsSnapshotToInfo(snap)
		if info.ReadyToUse {
			t.Error("ReadyToUse should be false for pending snapshot")
		}
	})

	t.Run("nil volume size yields zero bytes", func(t *testing.T) {
		snap := &ec2types.Snapshot{
			SnapshotId: aws.String("snap-nil"),
			Tags:       []ec2types.Tag{},
		}
		info := p.ebsSnapshotToInfo(snap)
		if info.SizeBytes != 0 {
			t.Errorf("SizeBytes = %d, want 0", info.SizeBytes)
		}
	})

	t.Run("missing tags yield empty IDs", func(t *testing.T) {
		snap := &ec2types.Snapshot{
			SnapshotId: aws.String("snap-notags"),
			VolumeSize: aws.Int32(1),
			Tags:       []ec2types.Tag{},
		}
		info := p.ebsSnapshotToInfo(snap)
		if info.SnapshotID != "" {
			t.Errorf("SnapshotID = %q, want empty", info.SnapshotID)
		}
		if info.SourceVolumeID != "" {
			t.Errorf("SourceVolumeID = %q, want empty", info.SourceVolumeID)
		}
	})
}
