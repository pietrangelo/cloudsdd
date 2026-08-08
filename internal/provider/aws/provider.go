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
	"cloudsdd/internal/provider/compute"
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
	// Defense in depth against the Engine's own schedule validation
	// (RFC 012 §2.3): a provider driven directly, outside the Engine,
	// must still refuse a schedule it cannot honor.
	if _, err := resourceSchedule(r, policies); err != nil {
		return err
	}

	switch r.Type {
	case spec.ResourceTypeRelationalDatabase:
		if _, err := decodeRelationalDatabaseProperties(r.Properties); err != nil {
			return err
		}
		if r.Scope.Region == "" {
			return fmt.Errorf("aws: resource %q: %w", r.ID, ErrRegionRequired)
		}
		if err := validateAWSRegionFormat(r.Scope.Region); err != nil {
			return fmt.Errorf("aws: resource %q: %w", r.ID, err)
		}
		return validateRegionAllowed(r.Scope.Region, policies.AllowedRegions)

	case spec.ResourceTypeComputeInstance:
		if _, err := decodeComputeInstanceProperties(r.Properties); err != nil {
			return err
		}
		// compute_instance is the first ResourceType to consume
		// Scope.Zones (RFC 005 §2.4.3, RFC 013 §2.3).
		if _, err := compute.Zone(r.Scope.Zones); err != nil {
			return fmt.Errorf("aws: resource %q: %w", r.ID, err)
		}
		if r.Scope.Region == "" {
			return fmt.Errorf("aws: resource %q: %w", r.ID, ErrRegionRequired)
		}
		if err := validateAWSRegionFormat(r.Scope.Region); err != nil {
			return fmt.Errorf("aws: resource %q: %w", r.ID, err)
		}
		return validateRegionAllowed(r.Scope.Region, policies.AllowedRegions)

	case spec.ResourceTypeContainerService:
		if _, err := decodeContainerServiceProperties(r.Properties, policies.AllowedRegistries); err != nil {
			return fmt.Errorf("aws: resource %q: %w", r.ID, err)
		}
		// Fargate places tasks itself across the subnets it is given, so a
		// pinned zone is a placement the user asked for and will not get.
		if len(r.Scope.Zones) > 0 {
			return fmt.Errorf("aws: resource %q: %w", r.ID, ErrZonesNotSupported)
		}
		if r.Scope.Region == "" {
			return fmt.Errorf("aws: resource %q: %w", r.ID, ErrRegionRequired)
		}
		if err := validateAWSRegionFormat(r.Scope.Region); err != nil {
			return fmt.Errorf("aws: resource %q: %w", r.ID, err)
		}
		return validateRegionAllowed(r.Scope.Region, policies.AllowedRegions)

	case spec.ResourceTypeBuildPipeline:
		if _, err := decodeBuildPipelineProperties(r.Properties); err != nil {
			return fmt.Errorf("aws: resource %q: %w", r.ID, err)
		}
		// CodeBuild places the build container itself, and the registry is
		// regional. A pinned zone is a placement the user asked for and
		// will not get.
		if len(r.Scope.Zones) > 0 {
			return fmt.Errorf("aws: resource %q: %w", r.ID, ErrZonesNotSupported)
		}
		if r.Scope.Region == "" {
			return fmt.Errorf("aws: resource %q: %w", r.ID, ErrRegionRequired)
		}
		if err := validateAWSRegionFormat(r.Scope.Region); err != nil {
			return fmt.Errorf("aws: resource %q: %w", r.ID, err)
		}
		return validateRegionAllowed(r.Scope.Region, policies.AllowedRegions)

	case spec.ResourceTypeObjectStorage:
		if _, err := decodeS3Properties(r.Properties); err != nil {
			return err
		}
		if len(r.Scope.Zones) > 0 {
			return fmt.Errorf("aws: resource %q: %w", r.ID, ErrZonesNotSupported)
		}
		if r.Scope.Region == "" {
			return fmt.Errorf("aws: resource %q: %w", r.ID, ErrRegionRequired)
		}
		if err := validateAWSRegionFormat(r.Scope.Region); err != nil {
			return fmt.Errorf("aws: resource %q: %w", r.ID, err)
		}
		return validateRegionAllowed(r.Scope.Region, policies.AllowedRegions)

	case spec.ResourceTypeCrossAccountRole:
		if _, err := decodeCrossAccountRoleProperties(r.Properties); err != nil {
			return err
		}
		if r.Scope.EffectiveSealed() {
			return fmt.Errorf("aws: resource %q: %w", r.ID, ErrSealedCrossAccountRole)
		}
		if r.Scope.Region != "" || len(r.Scope.Regions) > 0 || len(r.Scope.Zones) > 0 {
			return fmt.Errorf("aws: resource %q: %w", r.ID, ErrGlobalResourceScoped)
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

	stack, err := p.upsertStack(ctx, r.Account, r.Scope.Environment, region, r.ID, program)
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

	stack, err := p.upsertStack(ctx, r.Account, r.Scope.Environment, region, r.ID, program)
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

	stack, err := p.upsertStack(ctx, r.Account, r.Scope.Environment, region, r.ID, program)
	if err != nil {
		return fmt.Errorf("aws: failed to prepare stack for resource %q: %w", r.ID, err)
	}
	if err := p.setRegionConfig(ctx, stack, region); err != nil {
		return err
	}

	if _, err := stack.Destroy(ctx); err != nil {
		return fmt.Errorf("aws: destroy failed for resource %q: %w", r.ID, err)
	}
	if err := stack.Workspace().RemoveStack(ctx, stackNameFor(r.Account, r.Scope.Environment, region, r.ID)); err != nil {
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
	case spec.ResourceTypeRelationalDatabase:
		dbp, err := decodeRelationalDatabaseProperties(r.Properties)
		if err != nil {
			return nil, "", err
		}
		rules, err := resourceSchedule(r, policies)
		if err != nil {
			return nil, "", err
		}
		region := r.Scope.Region
		program := func(ctx *pulumi.Context) error {
			opts, _, err := p.providerOpts(ctx, region)
			if err != nil {
				return err
			}
			// The scope's network was provisioned by the Engine before
			// this program runs (RFC 016 §2.2), so it is looked up
			// rather than created here.
			net, err := lookupScopeNetwork(ctx, r)
			if err != nil {
				return err
			}
			instance, err := declareRelationalDatabase(ctx, r.ID, *dbp, net, opts...)
			if err != nil {
				return err
			}
			// The power schedule lives in the same program, and so in
			// the same stack, as the instance it governs (RFC 012 §4.1).
			return declareDatabaseSchedule(ctx, r.ID, instance, rules, opts...)
		}
		return program, region, nil

	case spec.ResourceTypeComputeInstance:
		cp, err := decodeComputeInstanceProperties(r.Properties)
		if err != nil {
			return nil, "", err
		}
		zone, err := compute.Zone(r.Scope.Zones)
		if err != nil {
			return nil, "", fmt.Errorf("aws: resource %q: %w", r.ID, err)
		}
		rules, err := resourceSchedule(r, policies)
		if err != nil {
			return nil, "", err
		}
		region := r.Scope.Region
		program := func(ctx *pulumi.Context) error {
			opts, invokeOpts, err := p.providerOpts(ctx, region)
			if err != nil {
				return err
			}
			net, err := lookupScopeNetwork(ctx, r)
			if err != nil {
				return err
			}
			instance, err := declareComputeInstance(ctx, r.ID, *cp, zone, net, invokeOpts, opts...)
			if err != nil {
				return err
			}
			return declareComputeSchedule(ctx, r.ID, instance, rules, opts...)
		}
		return program, region, nil

	case spec.ResourceTypeContainerService:
		cp, err := decodeContainerServiceProperties(r.Properties, policies.AllowedRegistries)
		if err != nil {
			return nil, "", fmt.Errorf("aws: resource %q: %w", r.ID, err)
		}
		rules, err := resourceSchedule(r, policies)
		if err != nil {
			return nil, "", err
		}
		region := r.Scope.Region
		program := func(ctx *pulumi.Context) error {
			opts, _, err := p.providerOpts(ctx, region)
			if err != nil {
				return err
			}
			net, err := lookupScopeNetwork(ctx, r)
			if err != nil {
				return err
			}
			_, err = declareContainerService(ctx, r, net, *cp, rules, opts...)
			return err
		}
		return program, region, nil

	case spec.ResourceTypeObjectStorage:
		s3p, err := decodeS3Properties(r.Properties)
		if err != nil {
			return nil, "", err
		}
		region := r.Scope.Region
		program := func(ctx *pulumi.Context) error {
			opts, _, err := p.providerOpts(ctx, region)
			if err != nil {
				return err
			}
			return declareS3Bucket(ctx, r.ID, *s3p, opts...)
		}
		return program, region, nil

	case spec.ResourceTypeBuildPipeline:
		bpp, err := decodeBuildPipelineProperties(r.Properties)
		if err != nil {
			return nil, "", err
		}
		region := r.Scope.Region
		program := func(ctx *pulumi.Context) error {
			opts, _, err := p.providerOpts(ctx, region)
			if err != nil {
				return err
			}
			// The build runs in CodeBuild's own managed network, not in
			// the scope's: it needs the internet to clone a public
			// repository and to reach the registry, and putting it in a
			// private subnet would mean routing both through the scope's
			// NAT for no gain. Nothing it produces is reachable from it.
			_, err = declareBuildPipeline(ctx, r.ID, *bpp, opts...)
			return err
		}
		return program, region, nil

	case spec.ResourceTypeCrossAccountRole:
		carp, err := decodeCrossAccountRoleProperties(r.Properties)
		if err != nil {
			return nil, "", err
		}
		allowedRegions := policies.AllowedRegions
		program := func(ctx *pulumi.Context) error {
			opts, _, err := p.providerOpts(ctx, "")
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
