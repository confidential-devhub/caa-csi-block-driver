// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"

	provider "github.com/confidential-devhub/caa-csi-block-driver/pkg/provider"
)

var logger = log.New(log.Writer(), "[caa-csi/aws] ", log.LstdFlags|log.Lmsgprefix)

func init() {
	provider.RegisterProvider("aws", func(params map[string]string) (provider.BlockVolumeProvider, error) {
		return NewAWSProvider(params)
	})
}

const (
	defaultVolumeType = "gp3"
	volumeTagKey      = "caa-csi-volume-id"
	waitTimeout       = 2 * time.Minute
)

type Config struct {
	Region           string
	AvailabilityZone string
	VolumeType       string
	AccessKeyId      string
	SecretKey        string
	IOPS             int32
	Throughput       int32
	KmsKeyId         string
}

// AWSProvider creates and deletes EBS volumes via the AWS EC2 API.
type AWSProvider struct {
	ec2Client *ec2.Client
	config    Config
}

// NewAWSProvider creates an AWSProvider from StorageClass parameters.
func NewAWSProvider(params map[string]string) (*AWSProvider, error) {
	region := params["awsRegion"]
	if region == "" {
		return nil, fmt.Errorf("awsRegion is required for aws provider")
	}

	az := params["awsAvailabilityZone"]
	if az == "" {
		return nil, fmt.Errorf("awsAvailabilityZone is required for aws provider")
	}

	volType := params["awsVolumeType"]
	if volType == "" {
		volType = defaultVolumeType
	}

	var iops int32
	if v := params["awsIops"]; v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid awsIops %q: must be a positive integer within int32 range", v)
		}
		iops = int32(n)
	}
	var throughput int32
	if v := params["awsThroughput"]; v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid awsThroughput %q: must be a positive integer within int32 range", v)
		}
		throughput = int32(n)
	}

	if iops > 0 && volType != "gp3" && volType != "io1" && volType != "io2" {
		return nil, fmt.Errorf("awsIops is only supported for gp3, io1, io2 volume types (got %s)", volType)
	}
	if throughput > 0 && volType != "gp3" {
		return nil, fmt.Errorf("awsThroughput is only supported for gp3 volume type (got %s)", volType)
	}

	cfg := Config{
		Region:           region,
		AvailabilityZone: az,
		VolumeType:       volType,
		AccessKeyId:      params["awsAccessKeyId"],
		SecretKey:        params["awsSecretKey"],
		IOPS:             iops,
		Throughput:       throughput,
		KmsKeyId:         params["awsKmsKeyId"],
	}

	client, err := newEC2Client(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create EC2 client: %w", err)
	}

	return &AWSProvider{
		ec2Client: client,
		config:    cfg,
	}, nil
}

func newEC2Client(cfg Config) (*ec2.Client, error) {
	var awsCfg aws.Config
	var err error

	if cfg.AccessKeyId != "" && cfg.SecretKey != "" {
		awsCfg, err = awsconfig.LoadDefaultConfig(context.TODO(),
			awsconfig.WithCredentialsProvider(
				credentials.NewStaticCredentialsProvider(cfg.AccessKeyId, cfg.SecretKey, "")),
			awsconfig.WithRegion(cfg.Region))
	} else {
		awsCfg, err = awsconfig.LoadDefaultConfig(context.TODO(),
			awsconfig.WithRegion(cfg.Region))
	}
	if err != nil {
		return nil, err
	}

	return ec2.NewFromConfig(awsCfg), nil
}

func (p *AWSProvider) CreateVolume(volumeID string, sizeBytes int64) (*provider.VolumeInfo, error) {
	ctx := context.TODO()

	exists, err := p.VolumeExists(volumeID)
	if err != nil {
		return nil, err
	}
	if exists {
		logger.Printf("Volume %s already exists, reusing", volumeID)
		return p.GetVolumeInfo(volumeID)
	}

	sizeGiB := int32(sizeBytes / (1024 * 1024 * 1024))
	if sizeGiB == 0 {
		sizeGiB = 1
	}

	logger.Printf("Creating EBS volume %s (%d GiB, type=%s, az=%s)",
		volumeID, sizeGiB, p.config.VolumeType, p.config.AvailabilityZone)

	input := &ec2.CreateVolumeInput{
		AvailabilityZone: aws.String(p.config.AvailabilityZone),
		Size:             aws.Int32(sizeGiB),
		VolumeType:       ec2types.VolumeType(p.config.VolumeType),
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeVolume,
			Tags: []ec2types.Tag{
				{Key: aws.String("Name"), Value: aws.String("csi-vol-" + volumeID)},
				{Key: aws.String(volumeTagKey), Value: aws.String(volumeID)},
			},
		}},
	}
	if p.config.IOPS > 0 {
		input.Iops = aws.Int32(p.config.IOPS)
	}
	if p.config.Throughput > 0 {
		input.Throughput = aws.Int32(p.config.Throughput)
	}
	if p.config.KmsKeyId != "" {
		input.Encrypted = aws.Bool(true)
		input.KmsKeyId = aws.String(p.config.KmsKeyId)
	}

	result, err := p.ec2Client.CreateVolume(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("ec2.CreateVolume failed for %s: %w", volumeID, err)
	}

	ebsVolumeID := *result.VolumeId
	logger.Printf("Created EBS volume %s (ebs-id=%s)", volumeID, ebsVolumeID)

	waiter := ec2.NewVolumeAvailableWaiter(p.ec2Client)
	if err := waiter.Wait(ctx, &ec2.DescribeVolumesInput{
		VolumeIds: []string{ebsVolumeID},
	}, waitTimeout); err != nil {
		logger.Printf("Warning: EBS volume %s did not become available within timeout: %v", ebsVolumeID, err)
	}

	return &provider.VolumeInfo{
		VolumeID:  volumeID,
		Path:      ebsVolumeID,
		SizeBytes: sizeBytes,
		Provider:  "aws",
		Metadata: map[string]string{
			"cloud-volume-path":  ebsVolumeID,
			"cloud-provider":     "aws",
			"ebs-volume-id":      ebsVolumeID,
			"availability-zone":  p.config.AvailabilityZone,
		},
	}, nil
}

func (p *AWSProvider) DeleteVolume(volumeID string) error {
	ctx := context.TODO()

	ebsVolumeID, err := p.findEBSVolumeID(volumeID)
	if err != nil {
		logger.Printf("Volume %s not found, nothing to delete", volumeID)
		return nil
	}

	logger.Printf("Deleting EBS volume %s (ebs-id=%s)", volumeID, ebsVolumeID)

	_, err = p.ec2Client.DeleteVolume(ctx, &ec2.DeleteVolumeInput{
		VolumeId: aws.String(ebsVolumeID),
	})
	if err != nil {
		return fmt.Errorf("ec2.DeleteVolume failed for %s: %w", ebsVolumeID, err)
	}

	logger.Printf("Deleted EBS volume %s", ebsVolumeID)
	return nil
}

func (p *AWSProvider) GetVolumeInfo(volumeID string) (*provider.VolumeInfo, error) {
	ctx := context.TODO()

	ebsVolumeID, err := p.findEBSVolumeID(volumeID)
	if err != nil {
		return nil, err
	}

	result, err := p.ec2Client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
		VolumeIds: []string{ebsVolumeID},
	})
	if err != nil {
		return nil, fmt.Errorf("ec2.DescribeVolumes failed for %s: %w", ebsVolumeID, err)
	}

	if len(result.Volumes) == 0 {
		return nil, fmt.Errorf("volume %s not found", volumeID)
	}

	vol := result.Volumes[0]
	return &provider.VolumeInfo{
		VolumeID:  volumeID,
		Path:      ebsVolumeID,
		SizeBytes: int64(*vol.Size) * 1024 * 1024 * 1024,
		Provider:  "aws",
		Metadata: map[string]string{
			"cloud-volume-path":  ebsVolumeID,
			"cloud-provider":     "aws",
			"ebs-volume-id":      ebsVolumeID,
			"availability-zone":  aws.ToString(vol.AvailabilityZone),
		},
	}, nil
}

func (p *AWSProvider) VolumeExists(volumeID string) (bool, error) {
	_, err := p.findEBSVolumeID(volumeID)
	if err != nil {
		return false, nil
	}
	return true, nil
}

// findEBSVolumeID looks up the EBS volume ID by our custom tag.
func (p *AWSProvider) findEBSVolumeID(volumeID string) (string, error) {
	ctx := context.TODO()

	result, err := p.ec2Client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
		Filters: []ec2types.Filter{
			{
				Name:   aws.String("tag:" + volumeTagKey),
				Values: []string{volumeID},
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("ec2.DescribeVolumes failed: %w", err)
	}

	if len(result.Volumes) == 0 {
		return "", fmt.Errorf("EBS volume with tag %s=%s not found", volumeTagKey, volumeID)
	}

	return *result.Volumes[0].VolumeId, nil
}

// ExpandVolume resizes an existing EBS volume to newSizeBytes.
func (p *AWSProvider) ExpandVolume(volumeID string, newSizeBytes int64) error {
	ctx := context.TODO()

	ebsVolumeID, err := p.findEBSVolumeID(volumeID)
	if err != nil {
		return fmt.Errorf("cannot find EBS volume for %s: %w", volumeID, err)
	}

	const gib = 1024 * 1024 * 1024
	newSizeGiB := int32((newSizeBytes + gib - 1) / gib)
	if newSizeGiB == 0 {
		newSizeGiB = 1
	}

	descResult, err := p.ec2Client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
		VolumeIds: []string{ebsVolumeID},
	})
	if err != nil {
		return fmt.Errorf("ec2.DescribeVolumes failed for %s: %w", ebsVolumeID, err)
	}
	if len(descResult.Volumes) > 0 && aws.ToInt32(descResult.Volumes[0].Size) >= newSizeGiB {
		logger.Printf("EBS volume %s already at %d GiB >= requested %d GiB, skipping ModifyVolume",
			ebsVolumeID, aws.ToInt32(descResult.Volumes[0].Size), newSizeGiB)
		return nil
	}

	logger.Printf("Expanding EBS volume %s (ebs-id=%s) to %d GiB", volumeID, ebsVolumeID, newSizeGiB)

	_, err = p.ec2Client.ModifyVolume(ctx, &ec2.ModifyVolumeInput{
		VolumeId: aws.String(ebsVolumeID),
		Size:     aws.Int32(newSizeGiB),
	})
	if err != nil {
		return fmt.Errorf("ec2.ModifyVolume failed for %s: %w", ebsVolumeID, err)
	}

	logger.Printf("EBS volume %s expand request accepted (modification is async, node resize will follow)", ebsVolumeID)
	return nil
}

// CreateSnapshot creates an EBS snapshot from the given volume.
func (p *AWSProvider) CreateSnapshot(ctx context.Context, volumeID, snapshotID string) (*provider.SnapshotInfo, error) {
	ebsVolumeID, err := p.findEBSVolumeID(volumeID)
	if err != nil {
		return nil, fmt.Errorf("cannot find EBS volume for snapshot: %w", err)
	}

	logger.Printf("Creating snapshot %s from EBS volume %s (ebs-id=%s)", snapshotID, volumeID, ebsVolumeID)

	result, err := p.ec2Client.CreateSnapshot(ctx, &ec2.CreateSnapshotInput{
		VolumeId: aws.String(ebsVolumeID),
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeSnapshot,
			Tags: []ec2types.Tag{
				{Key: aws.String("Name"), Value: aws.String("csi-snap-" + snapshotID)},
				{Key: aws.String("caa-csi-snapshot-id"), Value: aws.String(snapshotID)},
				{Key: aws.String(volumeTagKey), Value: aws.String(volumeID)},
			},
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("ec2.CreateSnapshot failed: %w", err)
	}

	var sizeBytes int64
	if result.VolumeSize != nil {
		sizeBytes = int64(*result.VolumeSize) * 1024 * 1024 * 1024
	}

	return &provider.SnapshotInfo{
		SnapshotID:     snapshotID,
		SourceVolumeID: volumeID,
		SizeBytes:      sizeBytes,
		CreationTime:   safeUnix(result.StartTime),
		ReadyToUse:     result.State == ec2types.SnapshotStateCompleted,
	}, nil
}

// DeleteSnapshot deletes an EBS snapshot by its CSI snapshot ID tag.
func (p *AWSProvider) DeleteSnapshot(ctx context.Context, snapshotID string) error {
	ebsSnapID, err := p.findEBSSnapshotID(snapshotID)
	if err != nil {
		logger.Printf("Snapshot %s not found, nothing to delete", snapshotID)
		return nil
	}

	logger.Printf("Deleting EBS snapshot %s (ebs-snap-id=%s)", snapshotID, ebsSnapID)
	_, err = p.ec2Client.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{
		SnapshotId: aws.String(ebsSnapID),
	})
	if err != nil {
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) && apiErr.ErrorCode() == "InvalidSnapshot.NotFound" {
			logger.Printf("Snapshot %s already deleted (EBS: %s)", snapshotID, ebsSnapID)
			return nil
		}
		return fmt.Errorf("ec2.DeleteSnapshot failed for %s: %w", ebsSnapID, err)
	}
	return nil
}

// ListSnapshots lists EBS snapshots. If volumeID is non-empty, only snapshots
// for that volume are returned; otherwise all managed snapshots are listed.
// Uses pagination to handle large numbers of snapshots.
func (p *AWSProvider) ListSnapshots(ctx context.Context, volumeID string) ([]*provider.SnapshotInfo, error) {
	var filters []ec2types.Filter
	if volumeID != "" {
		filters = append(filters, ec2types.Filter{
			Name:   aws.String("tag:" + volumeTagKey),
			Values: []string{volumeID},
		})
	} else {
		filters = append(filters, ec2types.Filter{
			Name:   aws.String("tag-key"),
			Values: []string{"caa-csi-snapshot-id"},
		})
	}

	var snaps []*provider.SnapshotInfo
	paginator := ec2.NewDescribeSnapshotsPaginator(p.ec2Client, &ec2.DescribeSnapshotsInput{
		Filters: filters,
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("ec2.DescribeSnapshots failed: %w", err)
		}
		for _, s := range page.Snapshots {
			snaps = append(snaps, p.ebsSnapshotToInfo(&s))
		}
	}
	return snaps, nil
}

// FindSnapshot looks up a single snapshot by its CSI snapshot name tag.
// Returns nil, nil if the snapshot does not exist.
func (p *AWSProvider) FindSnapshot(ctx context.Context, snapshotID string) (*provider.SnapshotInfo, error) {
	result, err := p.ec2Client.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{
		Filters: []ec2types.Filter{
			{
				Name:   aws.String("tag:caa-csi-snapshot-id"),
				Values: []string{snapshotID},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("ec2.DescribeSnapshots failed: %w", err)
	}
	if len(result.Snapshots) == 0 {
		return nil, nil
	}
	return p.ebsSnapshotToInfo(&result.Snapshots[0]), nil
}

func (p *AWSProvider) ebsSnapshotToInfo(s *ec2types.Snapshot) *provider.SnapshotInfo {
	snapID := ""
	sourceVolID := ""
	for _, tag := range s.Tags {
		switch aws.ToString(tag.Key) {
		case "caa-csi-snapshot-id":
			snapID = aws.ToString(tag.Value)
		case volumeTagKey:
			sourceVolID = aws.ToString(tag.Value)
		}
	}
	var sizeBytes int64
	if s.VolumeSize != nil {
		sizeBytes = int64(*s.VolumeSize) * 1024 * 1024 * 1024
	}
	return &provider.SnapshotInfo{
		SnapshotID:     snapID,
		SourceVolumeID: sourceVolID,
		SizeBytes:      sizeBytes,
		CreationTime:   safeUnix(s.StartTime),
		ReadyToUse:     s.State == ec2types.SnapshotStateCompleted,
	}
}

// CreateVolumeFromSnapshot creates a new EBS volume from an existing snapshot.
func (p *AWSProvider) CreateVolumeFromSnapshot(volumeID, snapshotID string, sizeBytes int64) (*provider.VolumeInfo, error) {
	ctx := context.TODO()

	ebsSnapID, err := p.findEBSSnapshotID(snapshotID)
	if err != nil {
		return nil, fmt.Errorf("cannot find snapshot %s: %w", snapshotID, err)
	}

	sizeGiB := int32(sizeBytes / (1024 * 1024 * 1024))
	if sizeGiB == 0 {
		sizeGiB = 1
	}

	logger.Printf("Creating EBS volume %s from snapshot %s (%d GiB)", volumeID, ebsSnapID, sizeGiB)

	input := &ec2.CreateVolumeInput{
		AvailabilityZone: aws.String(p.config.AvailabilityZone),
		SnapshotId:       aws.String(ebsSnapID),
		Size:             aws.Int32(sizeGiB),
		VolumeType:       ec2types.VolumeType(p.config.VolumeType),
		TagSpecifications: []ec2types.TagSpecification{{
			ResourceType: ec2types.ResourceTypeVolume,
			Tags: []ec2types.Tag{
				{Key: aws.String("Name"), Value: aws.String("csi-vol-" + volumeID)},
				{Key: aws.String(volumeTagKey), Value: aws.String(volumeID)},
			},
		}},
	}
	if p.config.KmsKeyId != "" {
		input.Encrypted = aws.Bool(true)
		input.KmsKeyId = aws.String(p.config.KmsKeyId)
	}

	result, err := p.ec2Client.CreateVolume(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("creating volume from snapshot %s: %w", ebsSnapID, err)
	}

	ebsVolumeID := *result.VolumeId
	waiter := ec2.NewVolumeAvailableWaiter(p.ec2Client)
	waiter.Wait(ctx, &ec2.DescribeVolumesInput{VolumeIds: []string{ebsVolumeID}}, waitTimeout) //nolint:errcheck

	return &provider.VolumeInfo{
		VolumeID:  volumeID,
		Path:      ebsVolumeID,
		SizeBytes: sizeBytes,
		Provider:  "aws",
		Metadata: map[string]string{
			"cloud-volume-path": ebsVolumeID,
			"cloud-provider":    "aws",
			"ebs-volume-id":     ebsVolumeID,
			"source-snapshot":   snapshotID,
		},
	}, nil
}

// CreateVolumeFromVolume creates a new EBS volume by first taking a
// snapshot of the source, then creating from that snapshot.
// The temporary snapshot is tagged for garbage collection.
func (p *AWSProvider) CreateVolumeFromVolume(volumeID, sourceVolumeID string, sizeBytes int64) (*provider.VolumeInfo, error) {
	ctx := context.TODO()
	tempSnapID := "clone-" + volumeID
	logger.Printf("Cloning volume %s → %s via temporary snapshot %s", sourceVolumeID, volumeID, tempSnapID)

	snapInfo, err := p.CreateSnapshot(ctx, sourceVolumeID, tempSnapID)
	if err != nil {
		return nil, fmt.Errorf("creating temp snapshot for clone: %w", err)
	}

	ebsSnapID, _ := p.findEBSSnapshotID(tempSnapID)
	if ebsSnapID != "" {
		ttl := time.Now().UTC().Add(1 * time.Hour).Format(time.RFC3339)
		_, tagErr := p.ec2Client.CreateTags(ctx, &ec2.CreateTagsInput{
			Resources: []string{ebsSnapID},
			Tags: []ec2types.Tag{
				{Key: aws.String("caa-csi-temp-snapshot"), Value: aws.String("true")},
				{Key: aws.String("caa-csi-temp-ttl"), Value: aws.String(ttl)},
			},
		})
		if tagErr != nil {
			logger.Printf("WARNING: failed to tag temp snapshot %s for GC: %v", ebsSnapID, tagErr)
		}

		snapWaiter := ec2.NewSnapshotCompletedWaiter(p.ec2Client)
		if err := snapWaiter.Wait(ctx, &ec2.DescribeSnapshotsInput{
			SnapshotIds: []string{ebsSnapID},
		}, waitTimeout); err != nil {
			logger.Printf("WARNING: snapshot %s did not complete in time: %v", ebsSnapID, err)
		}
	}

	volInfo, err := p.CreateVolumeFromSnapshot(volumeID, snapInfo.SnapshotID, sizeBytes)
	if err != nil {
		p.DeleteSnapshot(ctx, tempSnapID) //nolint:errcheck
		return nil, fmt.Errorf("creating volume from temp snapshot: %w", err)
	}

	go func() {
		time.Sleep(30 * time.Second)
		if err := p.DeleteSnapshot(context.Background(), tempSnapID); err != nil {
			logger.Printf("WARNING: failed to clean up temp clone snapshot %s: %v", tempSnapID, err)
		}
	}()

	return volInfo, nil
}

// CleanupOrphanedTempSnapshots deletes any EBS snapshots tagged as
// temporary whose TTL has expired.
func (p *AWSProvider) CleanupOrphanedTempSnapshots() {
	ctx := context.TODO()
	result, err := p.ec2Client.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{
		Filters: []ec2types.Filter{
			{Name: aws.String("tag:caa-csi-temp-snapshot"), Values: []string{"true"}},
		},
	})
	if err != nil {
		logger.Printf("WARNING: failed to list temp snapshots for cleanup: %v", err)
		return
	}

	now := time.Now().UTC()
	cleaned := 0
	for _, snap := range result.Snapshots {
		var ttlStr string
		for _, t := range snap.Tags {
			if aws.ToString(t.Key) == "caa-csi-temp-ttl" {
				ttlStr = aws.ToString(t.Value)
			}
		}
		if ttlStr == "" {
			continue
		}
		ttl, err := time.Parse(time.RFC3339, ttlStr)
		if err != nil {
			continue
		}
		if now.After(ttl) {
			snapID := aws.ToString(snap.SnapshotId)
			logger.Printf("Cleaning up orphaned temp snapshot %s (TTL %s expired)", snapID, ttlStr)
			if _, err := p.ec2Client.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{
				SnapshotId: aws.String(snapID),
			}); err != nil {
				logger.Printf("WARNING: failed to delete orphaned temp snapshot %s: %v", snapID, err)
			} else {
				cleaned++
			}
		}
	}
	if cleaned > 0 {
		logger.Printf("Cleaned up %d orphaned temp snapshot(s)", cleaned)
	}
}

func (p *AWSProvider) findEBSSnapshotID(snapshotID string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()

	result, err := p.ec2Client.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{
		Filters: []ec2types.Filter{
			{
				Name:   aws.String("tag:caa-csi-snapshot-id"),
				Values: []string{snapshotID},
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("ec2.DescribeSnapshots failed: %w", err)
	}
	if len(result.Snapshots) == 0 {
		return "", fmt.Errorf("snapshot with tag caa-csi-snapshot-id=%s not found", snapshotID)
	}
	if len(result.Snapshots) > 1 {
		return "", fmt.Errorf("ambiguous: found %d snapshots with tag caa-csi-snapshot-id=%s", len(result.Snapshots), snapshotID)
	}
	return aws.ToString(result.Snapshots[0].SnapshotId), nil
}

func safeUnix(t *time.Time) int64 {
	if t == nil {
		return 0
	}
	return t.Unix()
}
