// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package aws

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/provider"
	"cloudsdd/internal/spec"
)

// stateDirEnv indicates where the local Pulumi state backend lives (RFC
// 002 §2.3). If absent, the default ~/.cloudsdd/state is used.
const stateDirEnv = "CLOUDSDD_STATE_DIR"

// passphraseEnv holds the *name* of the environment variable that carries
// the Pulumi secrets provider passphrase (RFC 002 §2.3): mandatory, no
// weak default.
const passphraseEnv = "CLOUDSDD_PULUMI_PASSPHRASE" // #nosec G101 -- this is an env var name, not a credential

// pulumiProjectName is the logical Pulumi project every stack lives in
// (one per Resource.ID, RFC 002 §2.3).
const pulumiProjectName = "cloudsdd-aws"

// assumedCredentials represents AWS credentials assumed via STS for a
// DeploymentTarget (RFC 004 §4). They are always short-lived: never
// written to disk, they only live in memory for the duration of the
// process.
type assumedCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// AWSProvider implements provider.CloudProvider for AWS (RFC 002) using
// the Pulumi Automation API. An instance with creds == nil uses the
// default credential chain of the Pulumi AWS provider (RFC 002 §2.4); an
// instance with creds != nil (built by NewTargetProviderFactory, RFC 004
// §4) applies resources with credentials assumed for a specific
// DeploymentTarget.
type AWSProvider struct {
	stateDir   string
	passphrase string
	creds      *assumedCredentials
}

// NewProvider builds the default AWSProvider (AWS credentials from the
// standard credential chain, RFC 002 §2.4). Fails explicitly if
// CLOUDSDD_PULUMI_PASSPHRASE is not set, instead of using a weak default
// (RFC 002 §2.3).
func NewProvider() (*AWSProvider, error) {
	passphrase := os.Getenv(passphraseEnv)
	if passphrase == "" {
		return nil, ErrMissingPassphrase
	}

	stateDir := os.Getenv(stateDirEnv)
	if stateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("aws: failed to resolve default state dir: %w", err)
		}
		stateDir = filepath.Join(home, ".cloudsdd", "state")
	}
	// stateDir comes from an environment variable set by whoever operates
	// the CloudSDD process, not from the SDD Specification (the hostile
	// input per RFC 001 §3's threat model): filepath.Clean normalizes the
	// path for defensive hygiene, not to block an untrusted user.
	stateDir = filepath.Clean(stateDir)
	if err := os.MkdirAll(stateDir, 0o700); err != nil { // #nosec G703 -- stateDir is operator-controlled (env var), not Specification input
		return nil, fmt.Errorf("aws: failed to create state dir %q: %w", stateDir, err)
	}

	return &AWSProvider{stateDir: stateDir, passphrase: passphrase}, nil
}

func (p *AWSProvider) Name() string { return string(spec.ProviderAWS) }

var _ provider.CloudProvider = (*AWSProvider)(nil)

// Validate decodes and validates the Properties for the requested
// ResourceType and applies the Policies constraints, without making any
// call to AWS/Pulumi.
func (p *AWSProvider) Validate(ctx context.Context, r spec.Resource, policies spec.Policies) error {
	switch r.Type {
	case spec.ResourceTypeObjectStorage:
		s3p, err := decodeS3Properties(r.Properties)
		if err != nil {
			return err
		}
		return validateRegionAllowed(s3p.Region, policies.AllowedRegions)

	case spec.ResourceTypeCrossAccountRole:
		if _, err := decodeCrossAccountRoleProperties(r.Properties); err != nil {
			return err
		}
		if len(policies.AllowedRegions) == 0 {
			return fmt.Errorf("aws: resource %q: %w", r.ID, ErrMissingRegionPolicy)
		}
		return nil

	default:
		return fmt.Errorf("aws: resource %q: %w: %q", r.ID, ErrUnsupportedResourceType, r.Type)
	}
}

// Plan computes the Diff for the resource via Pulumi Preview (RFC 002
// §2.7), without applying any change.
func (p *AWSProvider) Plan(ctx context.Context, r spec.Resource, policies spec.Policies) (provider.Diff, error) {
	if err := p.Validate(ctx, r, policies); err != nil {
		return provider.Diff{}, err
	}

	program, region, err := p.resourceProgram(r, policies)
	if err != nil {
		return provider.Diff{}, err
	}

	stack, err := p.upsertStack(ctx, r.ID, program)
	if err != nil {
		return provider.Diff{}, fmt.Errorf("aws: failed to prepare stack for resource %q: %w", r.ID, err)
	}
	if err := p.setRegionConfig(ctx, stack, region); err != nil {
		return provider.Diff{}, err
	}

	result, err := stack.Preview(ctx)
	if err != nil {
		return provider.Diff{}, fmt.Errorf("aws: preview failed for resource %q: %w", r.ID, err)
	}

	return provider.Diff{
		ResourceID: r.ID,
		Action:     summarizeChangeSummary(result.ChangeSummary),
		Changes:    changesFromSummary(result.ChangeSummary),
	}, nil
}

// Apply applies the resource via Pulumi Up (RFC 002 §2.7).
func (p *AWSProvider) Apply(ctx context.Context, r spec.Resource, policies spec.Policies) (provider.Result, error) {
	if err := p.Validate(ctx, r, policies); err != nil {
		return provider.Result{}, err
	}

	program, region, err := p.resourceProgram(r, policies)
	if err != nil {
		return provider.Result{}, err
	}

	stack, err := p.upsertStack(ctx, r.ID, program)
	if err != nil {
		return provider.Result{}, fmt.Errorf("aws: failed to prepare stack for resource %q: %w", r.ID, err)
	}
	if err := p.setRegionConfig(ctx, stack, region); err != nil {
		return provider.Result{}, err
	}

	upResult, err := stack.Up(ctx)
	if err != nil {
		return provider.Result{ResourceID: r.ID, Status: provider.StatusFailed},
			fmt.Errorf("aws: apply failed for resource %q: %w", r.ID, err)
	}

	return provider.Result{
		ResourceID: r.ID,
		Status:     provider.StatusApplied,
		Details:    detailsFromOutputs(upResult.Outputs),
	}, nil
}

// Destroy removes the resource via Pulumi Destroy, then deletes the stack
// so as not to leave orphaned empty stacks in the local backend (RFC 002
// §2.7).
func (p *AWSProvider) Destroy(ctx context.Context, r spec.Resource, policies spec.Policies) error {
	program, region, err := p.resourceProgram(r, policies)
	if err != nil {
		return err
	}

	stack, err := p.upsertStack(ctx, r.ID, program)
	if err != nil {
		return fmt.Errorf("aws: failed to prepare stack for resource %q: %w", r.ID, err)
	}
	if err := p.setRegionConfig(ctx, stack, region); err != nil {
		return err
	}

	if _, err := stack.Destroy(ctx); err != nil {
		return fmt.Errorf("aws: destroy failed for resource %q: %w", r.ID, err)
	}
	if err := stack.Workspace().RemoveStack(ctx, stackNameFor(r.ID)); err != nil {
		return fmt.Errorf("aws: failed to remove stack for resource %q: %w", r.ID, err)
	}
	return nil
}

// resourceProgram builds the inline Pulumi program for r and returns the
// region to configure on the stack (empty for ResourceTypes that do not
// have one of their own, e.g. cross_account_role: IAM is global, RFC 003
// §2.3).
func (p *AWSProvider) resourceProgram(r spec.Resource, policies spec.Policies) (program pulumi.RunFunc, region string, err error) {
	switch r.Type {
	case spec.ResourceTypeObjectStorage:
		s3p, err := decodeS3Properties(r.Properties)
		if err != nil {
			return nil, "", err
		}
		program := func(ctx *pulumi.Context) error {
			opts, err := p.providerOpts(ctx, s3p.Region)
			if err != nil {
				return err
			}
			return declareS3Bucket(ctx, r.ID, *s3p, opts...)
		}
		return program, s3p.Region, nil

	case spec.ResourceTypeCrossAccountRole:
		carp, err := decodeCrossAccountRoleProperties(r.Properties)
		if err != nil {
			return nil, "", err
		}
		allowedRegions := policies.AllowedRegions
		program := func(ctx *pulumi.Context) error {
			opts, err := p.providerOpts(ctx, "")
			if err != nil {
				return err
			}
			return declareCrossAccountRole(ctx, r.ID, *carp, allowedRegions, opts...)
		}
		return program, "", nil

	default:
		return nil, "", fmt.Errorf("aws: resource %q: %w: %q", r.ID, ErrUnsupportedResourceType, r.Type)
	}
}
