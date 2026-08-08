// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package gcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optpreview"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optup"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/provider"
	"cloudsdd/internal/provider/compute"
	"cloudsdd/internal/provider/pulumiutil"
	"cloudsdd/internal/spec"
)

const stateDirEnv = "CLOUDSDD_STATE_DIR"
const passphraseEnv = "CLOUDSDD_PULUMI_PASSPHRASE" // #nosec G101 -- this is an env var name, not a credential

// stackScopeSeparator joins the components of a composite stack name (RFC
// 005 §2.6), matching the AWS provider's scheme.
const stackScopeSeparator = "::"

type GCPProvider struct {
	stateDir   string
	passphrase string
}

func NewProvider() (*GCPProvider, error) {
	passphrase := os.Getenv(passphraseEnv)
	if passphrase == "" {
		return nil, fmt.Errorf("gcp: %s is required", passphraseEnv)
	}

	stateDir := os.Getenv(stateDirEnv)
	if stateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("gcp: failed to resolve default state dir: %w", err)
		}
		stateDir = filepath.Join(home, ".cloudsdd", "state")
	}
	stateDir = filepath.Clean(stateDir)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("gcp: failed to create state dir %q: %w", stateDir, err)
	}

	return &GCPProvider{stateDir: stateDir, passphrase: passphrase}, nil
}

func (p *GCPProvider) Name() string { return string(spec.ProviderGCP) }

var _ provider.CloudProvider = (*GCPProvider)(nil)

// Validate checks that the resource can be expressed by GCP and complies
// with the Specification's Policies, without calling real infrastructure.
//
// RFC 011 §1.1D1: this previously never consulted Policies.AllowedRegions,
// so a Specification pinning allowed_regions deployed anywhere at all on
// GCP. The Engine now enforces the policy centrally (RFC 011 §2.3); the
// call here is defense in depth for a provider driven directly.
func (p *GCPProvider) Validate(ctx context.Context, r spec.Resource, policies spec.Policies) error {
	// Defense in depth against the Engine's schedule validation (RFC 012
	// §2.3), plus the GCP-specific refusal of exception windows.
	if _, err := resourceSchedule(r, policies); err != nil {
		return err
	}

	switch r.Type {
	case spec.ResourceTypeObjectStorage:
		if _, err := decodeObjectStorageProperties(r.Properties); err != nil {
			return err
		}
	case spec.ResourceTypeRelationalDatabase:
		if _, err := decodeRelationalDatabaseProperties(r.Properties); err != nil {
			return err
		}
	case spec.ResourceTypeComputeInstance:
		if _, err := decodeComputeInstanceProperties(r.Properties); err != nil {
			return err
		}
		// The first ResourceType to consume Scope.Zones (RFC 005 §2.4.3,
		// RFC 013 §2.3).
		if _, err := compute.Zone(r.Scope.Zones); err != nil {
			return fmt.Errorf("gcp: resource %q: %w", r.ID, err)
		}
	case spec.ResourceTypeContainerService:
		if _, err := decodeContainerServiceProperties(r.Properties, policies.AllowedRegistries); err != nil {
			return fmt.Errorf("gcp: resource %q: %w", r.ID, err)
		}
		// Cloud Run is regional and places instances itself, so a zone
		// pinned here is a placement the user asked for and will not get.
		if len(r.Scope.Zones) > 0 {
			return fmt.Errorf("gcp: resource %q: %w", r.ID, ErrZonesNotSupported)
		}
	case spec.ResourceTypeBuildPipeline:
		if _, err := decodeBuildPipelineProperties(r.Properties); err != nil {
			return fmt.Errorf("gcp: resource %q: %w", r.ID, err)
		}
		// Cloud Build places the build container itself and Artifact
		// Registry is regional. A pinned zone is a placement the user asked
		// for and will not get.
		if len(r.Scope.Zones) > 0 {
			return fmt.Errorf("gcp: resource %q: %w", r.ID, ErrZonesNotSupported)
		}
	default:
		return fmt.Errorf("gcp: resource %q: unsupported resource type: %q", r.ID, r.Type)
	}

	if r.Scope.Region == "" {
		return fmt.Errorf("gcp: resource %q: region required", r.ID)
	}
	if err := validateGCPRegionFormat(r.Scope.Region); err != nil {
		return fmt.Errorf("gcp: resource %q: %w", r.ID, err)
	}
	if err := provider.ValidateRegionAllowed(r.Scope.Region, policies.AllowedRegions); err != nil {
		return fmt.Errorf("gcp: resource %q: %w", r.ID, err)
	}
	return nil
}

// stackNameFor mirrors the AWS scheme (RFC 005 §2.6): state isolation
// across Account/Environment/Region so two resources sharing an ID but
// scoped differently never collide in one Pulumi stack.
func stackNameFor(account, environment, region, resourceID string) string {
	parts := make([]string, 0, 4)
	for _, s := range []string{account, environment, region} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	parts = append(parts, resourceID)
	return strings.Join(parts, stackScopeSeparator)
}

func (p *GCPProvider) upsertStack(ctx context.Context, account, env, region, resourceID string, program pulumi.RunFunc) (auto.Stack, error) {
	s, err := auto.UpsertStackInlineSource(ctx, stackNameFor(account, env, region, resourceID), "cloudsdd-gcp", program,
		auto.SecretsProvider("passphrase"),
		auto.EnvVars(map[string]string{"PULUMI_CONFIG_PASSPHRASE": p.passphrase}),
		auto.WorkDir(p.stateDir),
	)
	if err != nil {
		return auto.Stack{}, fmt.Errorf("gcp: failed to upsert stack for %q: %w", resourceID, err)
	}
	if err := s.SetConfig(ctx, "gcp:region", auto.ConfigValue{Value: region}); err != nil {
		return auto.Stack{}, fmt.Errorf("gcp: failed to set region config: %w", err)
	}
	return s, nil
}

// resourceProgram builds the inline Pulumi program for r. Decode errors
// are propagated rather than discarded: the Azure twin dropped them and
// then dereferenced a nil pointer inside the closure (RFC 011 §1.1B1).
func (p *GCPProvider) resourceProgram(r spec.Resource, policies spec.Policies) (pulumi.RunFunc, string, error) {
	region := r.Scope.Region

	switch r.Type {
	case spec.ResourceTypeObjectStorage:
		props, err := decodeObjectStorageProperties(r.Properties)
		if err != nil {
			return nil, "", err
		}
		return func(ctx *pulumi.Context) error {
			return declareObjectStorage(ctx, r.ID, region, *props)
		}, region, nil

	case spec.ResourceTypeRelationalDatabase:
		props, err := decodeRelationalDatabaseProperties(r.Properties)
		if err != nil {
			return nil, "", err
		}
		rules, err := resourceSchedule(r, policies)
		if err != nil {
			return nil, "", err
		}
		netName := scopeNetworkName(resourceScope(r))
		return func(ctx *pulumi.Context) error {
			instance, err := declareRelationalDatabase(ctx, r.ID, region, netName, *props)
			if err != nil {
				return err
			}
			// The power schedule shares the instance's stack, so the
			// existing Destroy path removes both (RFC 012 §4.2).
			return declareDatabaseSchedule(ctx, r.ID, region, instance, rules)
		}, region, nil

	case spec.ResourceTypeComputeInstance:
		props, err := decodeComputeInstanceProperties(r.Properties)
		if err != nil {
			return nil, "", err
		}
		zone, err := compute.Zone(r.Scope.Zones)
		if err != nil {
			return nil, "", fmt.Errorf("gcp: resource %q: %w", r.ID, err)
		}
		rules, err := resourceSchedule(r, policies)
		if err != nil {
			return nil, "", err
		}
		netName := scopeNetworkName(resourceScope(r))
		return func(ctx *pulumi.Context) error {
			_, err := declareComputeInstance(ctx, r.ID, region, zone, netName, *props, rules)
			return err
		}, region, nil

	case spec.ResourceTypeContainerService:
		props, err := decodeContainerServiceProperties(r.Properties, policies.AllowedRegistries)
		if err != nil {
			return nil, "", fmt.Errorf("gcp: resource %q: %w", r.ID, err)
		}
		netName := scopeNetworkName(resourceScope(r))
		return func(ctx *pulumi.Context) error {
			_, err := declareContainerService(ctx, r, netName, *props)
			return err
		}, region, nil

	case spec.ResourceTypeBuildPipeline:
		props, err := decodeBuildPipelineProperties(r.Properties)
		if err != nil {
			return nil, "", fmt.Errorf("gcp: resource %q: %w", r.ID, err)
		}
		// The build runs in Cloud Build's own managed pool, not in the
		// scope's network: it needs the internet to clone a public
		// repository and to reach the registry, and putting it behind the
		// scope's egress would mean routing both through Cloud NAT for no
		// gain. Nothing it produces is reachable from it.
		return func(ctx *pulumi.Context) error {
			_, err := declareBuildPipeline(ctx, r.ID, region, *props, r.Resolved)
			return err
		}, region, nil

	default:
		return nil, "", fmt.Errorf("gcp: resource %q: unsupported resource type: %q", r.ID, r.Type)
	}
}

func (p *GCPProvider) Plan(ctx context.Context, r spec.Resource, policies spec.Policies) (provider.Diff, error) {
	if err := p.Validate(ctx, r, policies); err != nil {
		return provider.Diff{}, err
	}
	program, region, err := p.resourceProgram(r, policies)
	if err != nil {
		return provider.Diff{}, err
	}
	stack, err := p.upsertStack(ctx, r.Account, r.Scope.Environment, region, r.ID, program)
	if err != nil {
		return provider.Diff{}, err
	}
	res, err := stack.Preview(ctx, optpreview.Message("plan"))
	if err != nil {
		return provider.Diff{}, fmt.Errorf("gcp: preview failed for resource %q: %w", r.ID, err)
	}

	return provider.Diff{
		ResourceID: r.ID,
		Region:     region,
		Action:     pulumiutil.SummarizeChangeSummary(res.ChangeSummary),
		Changes:    pulumiutil.ChangesFromSummary(res.ChangeSummary),
	}, nil
}

func (p *GCPProvider) Apply(ctx context.Context, r spec.Resource, policies spec.Policies) (provider.Result, error) {
	if err := p.Validate(ctx, r, policies); err != nil {
		return provider.Result{}, err
	}
	program, region, err := p.resourceProgram(r, policies)
	if err != nil {
		return provider.Result{}, err
	}
	stack, err := p.upsertStack(ctx, r.Account, r.Scope.Environment, region, r.ID, program)
	if err != nil {
		return provider.Result{}, err
	}
	up, err := stack.Up(ctx, optup.Message("apply"))
	if err != nil {
		return provider.Result{ResourceID: r.ID, Region: region, Status: provider.StatusFailed}, fmt.Errorf("gcp: apply failed for resource %q: %w", r.ID, err)
	}
	return provider.Result{
		ResourceID: r.ID,
		Region:     region,
		Status:     provider.StatusApplied,
		Details:    pulumiutil.DetailsFromOutputs(up.Outputs),
	}, nil
}

// Destroy removes the resource from real infrastructure. It validates
// first: RFC 011 §1.1B1 found the Azure twin skipping this and panicking
// on any resource whose properties failed to decode.
func (p *GCPProvider) Destroy(ctx context.Context, r spec.Resource, policies spec.Policies) error {
	if err := p.Validate(ctx, r, policies); err != nil {
		return err
	}
	program, region, err := p.resourceProgram(r, policies)
	if err != nil {
		return err
	}
	stack, err := p.upsertStack(ctx, r.Account, r.Scope.Environment, region, r.ID, program)
	if err != nil {
		return err
	}
	if _, err := stack.Destroy(ctx); err != nil {
		return fmt.Errorf("gcp: destroy failed for resource %q: %w", r.ID, err)
	}
	return nil
}
