package aws

import (
	"context"
	"fmt"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"cloudsdd/internal/engine"
	"cloudsdd/internal/provider"
)

// defaultAssumeRoleSessionSeconds is used when
// AWSTargetConfig.SessionDurationSeconds is zero (RFC 004 §2.1: never
// long-lived credentials, but an explicit default instead of leaving the
// choice to the SDK's default).
const defaultAssumeRoleSessionSeconds = int32(3600)

// NewTargetProviderFactory builds an engine.TargetProviderFactory (RFC
// 004 §4) that, for every AWS DeploymentTarget, assumes the role described
// in target.AWS via STS and returns an AWSProvider scoped to those
// credentials, reusing the same base state backend/passphrase.
// base must already be initialized with NewProvider.
func NewTargetProviderFactory(base *AWSProvider) engine.TargetProviderFactory {
	return func(ctx context.Context, target engine.DeploymentTarget) (provider.CloudProvider, error) {
		if target.AWS == nil {
			return nil, fmt.Errorf("aws: deployment target %q has no AWS configuration", target.Name)
		}

		cfg, err := awsconfig.LoadDefaultConfig(ctx)
		if err != nil {
			return nil, fmt.Errorf("aws: failed to load default AWS config for target %q: %w", target.Name, err)
		}

		durationSeconds := target.AWS.SessionDurationSeconds
		if durationSeconds == 0 {
			durationSeconds = defaultAssumeRoleSessionSeconds
		}

		client := sts.NewFromConfig(cfg)
		out, err := client.AssumeRole(ctx, &sts.AssumeRoleInput{
			RoleArn:         awssdk.String(target.AWS.RoleARN),
			RoleSessionName: awssdk.String("cloudsdd-" + target.Name),
			ExternalId:      awssdk.String(target.AWS.ExternalID),
			DurationSeconds: awssdk.Int32(durationSeconds),
		})
		if err != nil {
			return nil, fmt.Errorf("aws: failed to assume role for target %q: %w", target.Name, err)
		}
		if out.Credentials == nil {
			return nil, fmt.Errorf("aws: assume role for target %q returned no credentials", target.Name)
		}

		return &AWSProvider{
			stateDir:   base.stateDir,
			passphrase: base.passphrase,
			creds: &assumedCredentials{
				AccessKeyID:     awssdk.ToString(out.Credentials.AccessKeyId),
				SecretAccessKey: awssdk.ToString(out.Credentials.SecretAccessKey),
				SessionToken:    awssdk.ToString(out.Credentials.SessionToken),
			},
		}, nil
	}
}
