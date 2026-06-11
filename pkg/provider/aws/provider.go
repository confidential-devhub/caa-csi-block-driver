// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"fmt"
	"log"
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

	cfg := Config{
		Region:           region,
		AvailabilityZone: az,
		VolumeType:       volType,
		AccessKeyId:      params["awsAccessKeyId"],
		SecretKey:        params["awsSecretKey"],
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
	var opts []func(*awsconfig.LoadOptions) error
	opts = append(opts, awsconfig.WithRegion(cfg.Region))

	if cfg.AccessKeyId != "" && cfg.SecretKey != "" {
		logger.Printf("Using static AWS credentials")
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyId, cfg.SecretKey, "")))
	} else {
		logger.Printf("Using default AWS credential chain (IRSA/instance profile/env)")
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(context.TODO(), opts...)
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
