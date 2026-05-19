// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

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
		if n, err := strconv.Atoi(v); err == nil {
			iops = int32(n)
		}
	}
	var throughput int32
	if v := params["awsThroughput"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			throughput = int32(n)
		}
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

// ExpandVolume resizes an existing EBS volume to newSizeBytes.
func (p *AWSProvider) ExpandVolume(volumeID string, newSizeBytes int64) error {
	ctx := context.TODO()

	ebsVolumeID, err := p.findEBSVolumeID(volumeID)
	if err != nil {
		return fmt.Errorf("cannot find EBS volume for %s: %w", volumeID, err)
	}

	newSizeGiB := int32(newSizeBytes / (1024 * 1024 * 1024))
	if newSizeGiB == 0 {
		newSizeGiB = 1
	}

	logger.Printf("Expanding EBS volume %s (ebs-id=%s) to %d GiB", volumeID, ebsVolumeID, newSizeGiB)

	_, err = p.ec2Client.ModifyVolume(ctx, &ec2.ModifyVolumeInput{
		VolumeId: aws.String(ebsVolumeID),
		Size:     aws.Int32(newSizeGiB),
	})
	if err != nil {
		return fmt.Errorf("ec2.ModifyVolume failed for %s: %w", ebsVolumeID, err)
	}

	logger.Printf("EBS volume %s expand request accepted", ebsVolumeID)
	return nil
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
func (p *AWSProvider) CreateVolumeFromVolume(volumeID, sourceVolumeID string, sizeBytes int64) (*provider.VolumeInfo, error) {
	tempSnapID := "clone-" + volumeID
	logger.Printf("Cloning volume %s → %s via temporary snapshot %s", sourceVolumeID, volumeID, tempSnapID)

	snapInfo, err := p.CreateSnapshot(sourceVolumeID, tempSnapID)
	if err != nil {
		return nil, fmt.Errorf("creating temp snapshot for clone: %w", err)
	}

	ctx := context.TODO()
	ebsSnapID, _ := p.findEBSSnapshotID(tempSnapID)
	if ebsSnapID != "" {
		snapWaiter := ec2.NewSnapshotCompletedWaiter(p.ec2Client)
		if err := snapWaiter.Wait(ctx, &ec2.DescribeSnapshotsInput{
			SnapshotIds: []string{ebsSnapID},
		}, waitTimeout); err != nil {
			logger.Printf("WARNING: snapshot %s did not complete in time: %v", ebsSnapID, err)
		}
	}

	volInfo, err := p.CreateVolumeFromSnapshot(volumeID, snapInfo.SnapshotID, sizeBytes)
	if err != nil {
		p.DeleteSnapshot(tempSnapID) //nolint:errcheck
		return nil, fmt.Errorf("creating volume from temp snapshot: %w", err)
	}

	go func() {
		time.Sleep(30 * time.Second)
		if err := p.DeleteSnapshot(tempSnapID); err != nil {
			logger.Printf("WARNING: failed to clean up temp clone snapshot %s: %v", tempSnapID, err)
		}
	}()

	return volInfo, nil
}

// CreateSnapshot creates an EBS snapshot from the given volume.
func (p *AWSProvider) CreateSnapshot(volumeID, snapshotID string) (*provider.SnapshotInfo, error) {
	ctx := context.TODO()

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
		CreationTime:   result.StartTime.Unix(),
		ReadyToUse:     result.State == ec2types.SnapshotStateCompleted,
	}, nil
}

// DeleteSnapshot deletes an EBS snapshot by its CSI snapshot ID tag.
func (p *AWSProvider) DeleteSnapshot(snapshotID string) error {
	ctx := context.TODO()

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
		return fmt.Errorf("ec2.DeleteSnapshot failed for %s: %w", ebsSnapID, err)
	}
	return nil
}

// ListSnapshots lists EBS snapshots for the given volume.
func (p *AWSProvider) ListSnapshots(volumeID string) ([]*provider.SnapshotInfo, error) {
	ctx := context.TODO()

	result, err := p.ec2Client.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{
		Filters: []ec2types.Filter{
			{
				Name:   aws.String("tag:" + volumeTagKey),
				Values: []string{volumeID},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("ec2.DescribeSnapshots failed: %w", err)
	}

	var snaps []*provider.SnapshotInfo
	for _, s := range result.Snapshots {
		snapID := ""
		for _, tag := range s.Tags {
			if aws.ToString(tag.Key) == "caa-csi-snapshot-id" {
				snapID = aws.ToString(tag.Value)
			}
		}
		var sizeBytes int64
		if s.VolumeSize != nil {
			sizeBytes = int64(*s.VolumeSize) * 1024 * 1024 * 1024
		}
		snaps = append(snaps, &provider.SnapshotInfo{
			SnapshotID:     snapID,
			SourceVolumeID: volumeID,
			SizeBytes:      sizeBytes,
			CreationTime:   s.StartTime.Unix(),
			ReadyToUse:     s.State == ec2types.SnapshotStateCompleted,
		})
	}
	return snaps, nil
}

// ListManagedVolumes returns all EBS volumes tagged with our CSI tag.
func (p *AWSProvider) ListManagedVolumes() ([]*provider.VolumeInfo, error) {
	ctx := context.TODO()

	result, err := p.ec2Client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
		Filters: []ec2types.Filter{
			{
				Name:   aws.String("tag-key"),
				Values: []string{volumeTagKey},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("ec2.DescribeVolumes for recovery failed: %w", err)
	}

	var vols []*provider.VolumeInfo
	for _, vol := range result.Volumes {
		csiVolumeID := ""
		for _, tag := range vol.Tags {
			if aws.ToString(tag.Key) == volumeTagKey {
				csiVolumeID = aws.ToString(tag.Value)
			}
		}
		if csiVolumeID == "" {
			continue
		}
		ebsID := aws.ToString(vol.VolumeId)
		vols = append(vols, &provider.VolumeInfo{
			VolumeID:  csiVolumeID,
			Path:      ebsID,
			SizeBytes: int64(aws.ToInt32(vol.Size)) * 1024 * 1024 * 1024,
			Provider:  "aws",
			Metadata: map[string]string{
				"cloud-volume-path": ebsID,
				"cloud-provider":    "aws",
				"ebs-volume-id":     ebsID,
				"availability-zone": aws.ToString(vol.AvailabilityZone),
			},
		})
	}
	return vols, nil
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

func (p *AWSProvider) findEBSSnapshotID(snapshotID string) (string, error) {
	ctx := context.TODO()

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
	return aws.ToString(result.Snapshots[0].SnapshotId), nil
}
