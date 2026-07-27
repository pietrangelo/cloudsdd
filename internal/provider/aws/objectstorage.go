package aws

import (
	"fmt"

	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/s3"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// S3Properties represents the Properties of a Resource with
// Type: "object_storage" on AWS (RFC 002 §2.6).
//
// Versioning, Encryption, and BlockPublicAccess are *bool, not bool: their
// absence activates a secure default (Effective*, below) instead of the
// zero-value of a plain bool, which for Encryption/BlockPublicAccess
// would be a silent insecure default.
type S3Properties struct {
	BucketName        string `json:"bucket_name" validate:"required,min=3,max=63,s3bucketname"`
	Region            string `json:"region" validate:"required,awsregion"`
	Versioning        *bool  `json:"versioning,omitempty"`
	Encryption        *bool  `json:"encryption,omitempty"`
	BlockPublicAccess *bool  `json:"block_public_access,omitempty"`
}

// EffectiveVersioning returns the value of Versioning, or false if absent:
// versioning carries a storage cost, not a security posture, so the
// default is "off" unless explicitly requested.
func (p S3Properties) EffectiveVersioning() bool { return boolOrDefault(p.Versioning, false) }

// EffectiveEncryption returns the value of Encryption, or true if absent
// (RFC 002 §2.6: secure by default).
func (p S3Properties) EffectiveEncryption() bool { return boolOrDefault(p.Encryption, true) }

// EffectiveBlockPublicAccess returns the value of BlockPublicAccess, or
// true if absent (RFC 002 §2.6: secure by default).
func (p S3Properties) EffectiveBlockPublicAccess() bool {
	return boolOrDefault(p.BlockPublicAccess, true)
}

// decodeS3Properties decodes and validates Properties as S3Properties (RFC
// 002 §2.6).
func decodeS3Properties(props map[string]any) (*S3Properties, error) {
	var p S3Properties
	if err := decodeProperties(props, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// declareS3Bucket registers, in the inline Pulumi program (RFC 002 §2.1,
// §2.6), the resources needed to express an S3 bucket with p's effective
// security posture: bucket, versioning, server-side encryption, and public
// access block as separate Pulumi resources (necessary because
// s3.BucketV2 no longer exposes them as inline arguments, unlike the
// legacy s3.Bucket).
// opts is forwarded to every declared Pulumi resource: empty when using
// the default credential chain (RFC 002 §2.4), or
// pulumi.Provider(<explicit>) when the resource is applied against a
// DeploymentTarget with assumed credentials (RFC 004 §4).
func declareS3Bucket(ctx *pulumi.Context, resourceID string, p S3Properties, opts ...pulumi.ResourceOption) error {
	bucket, err := s3.NewBucketV2(ctx, resourceID, &s3.BucketV2Args{
		Bucket: pulumi.String(p.BucketName),
	}, opts...)
	if err != nil {
		return fmt.Errorf("aws: failed to declare s3 bucket %q: %w", resourceID, err)
	}

	versioningStatus := "Disabled"
	if p.EffectiveVersioning() {
		versioningStatus = "Enabled"
	}
	if _, err := s3.NewBucketVersioningV2(ctx, resourceID+"-versioning", &s3.BucketVersioningV2Args{
		Bucket: bucket.ID(),
		VersioningConfiguration: &s3.BucketVersioningV2VersioningConfigurationArgs{
			Status: pulumi.String(versioningStatus),
		},
	}, opts...); err != nil {
		return fmt.Errorf("aws: failed to declare versioning for bucket %q: %w", resourceID, err)
	}

	if p.EffectiveEncryption() {
		if _, err := s3.NewBucketServerSideEncryptionConfigurationV2(ctx, resourceID+"-encryption", &s3.BucketServerSideEncryptionConfigurationV2Args{
			Bucket: bucket.ID(),
			Rules: s3.BucketServerSideEncryptionConfigurationV2RuleArray{
				&s3.BucketServerSideEncryptionConfigurationV2RuleArgs{
					ApplyServerSideEncryptionByDefault: &s3.BucketServerSideEncryptionConfigurationV2RuleApplyServerSideEncryptionByDefaultArgs{
						SseAlgorithm: pulumi.String("AES256"),
					},
				},
			},
		}, opts...); err != nil {
			return fmt.Errorf("aws: failed to declare encryption for bucket %q: %w", resourceID, err)
		}
	}

	if p.EffectiveBlockPublicAccess() {
		if _, err := s3.NewBucketPublicAccessBlock(ctx, resourceID+"-public-access-block", &s3.BucketPublicAccessBlockArgs{
			Bucket:                bucket.ID(),
			BlockPublicAcls:       pulumi.Bool(true),
			BlockPublicPolicy:     pulumi.Bool(true),
			IgnorePublicAcls:      pulumi.Bool(true),
			RestrictPublicBuckets: pulumi.Bool(true),
		}, opts...); err != nil {
			return fmt.Errorf("aws: failed to declare public access block for bucket %q: %w", resourceID, err)
		}
	}

	return nil
}
