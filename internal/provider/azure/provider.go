// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package azure

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
// 005 §2.6), matching the AWS and GCP providers' scheme.
const stackScopeSeparator = "::"

type AzureProvider struct {
	stateDir   string
	passphrase string
}

func NewProvider() (*AzureProvider, error) {
	passphrase := os.Getenv(passphraseEnv)
	if passphrase == "" {
		return nil, fmt.Errorf("azure: %s is required", passphraseEnv)
	}

	stateDir := os.Getenv(stateDirEnv)
	if stateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("azure: failed to resolve default state dir: %w", err)
		}
		stateDir = filepath.Join(home, ".cloudsdd", "state")
	}
	stateDir = filepath.Clean(stateDir)
	// Previously ignored, unlike the GCP twin which checked it (RFC 011
	// §1.1F3).
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("azure: failed to create state dir %q: %w", stateDir, err)
	}

	return &AzureProvider{stateDir: stateDir, passphrase: passphrase}, nil
}

func (p *AzureProvider) Name() string { return string(spec.ProviderAzure) }

var _ provider.CloudProvider = (*AzureProvider)(nil)

// Validate checks that the resource can be expressed by Azure and complies
// with the Specification's Policies, without calling real infrastructure.
//
// RFC 011 §1.1D1: this previously never consulted Policies.AllowedRegions.
func (p *AzureProvider) Validate(ctx context.Context, r spec.Resource, policies spec.Policies) error {
	// Defense in depth against the Engine's schedule validation
	// (RFC 012 §2.3).
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
			return fmt.Errorf("azure: resource %q: %w", r.ID, err)
		}
	case spec.ResourceTypeContainerService:
		if _, err := decodeContainerServiceProperties(r.Properties, policies.AllowedRegistries); err != nil {
			return fmt.Errorf("azure: resource %q: %w", r.ID, err)
		}
		// Container Apps places replicas itself across the environment, so
		// a pinned zone is a placement the user asked for and will not get.
		if len(r.Scope.Zones) > 0 {
			return fmt.Errorf("azure: resource %q: %w", r.ID, ErrZonesNotSupported)
		}
	case spec.ResourceTypeBuildPipeline:
		if _, err := decodeBuildPipelineProperties(r.Properties); err != nil {
			return fmt.Errorf("azure: resource %q: %w", r.ID, err)
		}
		// An ACR Task runs on a pool ACR places itself, and the registry is
		// regional. A pinned zone is a placement the user asked for and will
		// not get.
		if len(r.Scope.Zones) > 0 {
			return fmt.Errorf("azure: resource %q: %w", r.ID, ErrZonesNotSupported)
		}
	default:
		return fmt.Errorf("azure: resource %q: unsupported resource type: %q", r.ID, r.Type)
	}

	if r.Scope.Region == "" {
		return fmt.Errorf("azure: resource %q: region required", r.ID)
	}
	if err := validateAzureLocationFormat(r.Scope.Region); err != nil {
		return fmt.Errorf("azure: resource %q: %w", r.ID, err)
	}
	if err := provider.ValidateRegionAllowed(r.Scope.Region, policies.AllowedRegions); err != nil {
		return fmt.Errorf("azure: resource %q: %w", r.ID, err)
	}
	return nil
}

// stackNameFor mirrors the AWS scheme (RFC 005 §2.6).
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

func (p *AzureProvider) upsertStack(ctx context.Context, account, env, region, resourceID string, program pulumi.RunFunc) (auto.Stack, error) {
	s, err := auto.UpsertStackInlineSource(ctx, stackNameFor(account, env, region, resourceID), "cloudsdd-azure", program,
		auto.SecretsProvider("passphrase"),
		auto.EnvVars(map[string]string{"PULUMI_CONFIG_PASSPHRASE": p.passphrase}),
		auto.WorkDir(p.stateDir),
	)
	if err != nil {
		return auto.Stack{}, fmt.Errorf("azure: failed to upsert stack for %q: %w", resourceID, err)
	}
	if err := s.SetConfig(ctx, "azure:location", auto.ConfigValue{Value: region}); err != nil {
		return auto.Stack{}, fmt.Errorf("azure: failed to set location config: %w", err)
	}
	return s, nil
}

// resourceProgram builds the inline Pulumi program for r.
//
// RFC 011 §1.1B1: the decode errors here were previously discarded
// (`s3p, _ :=`) and the resulting nil pointer dereferenced inside the
// closure. Plan and Apply happened to call Validate first; Destroy did
// not, so `cloudsdd destroy` panicked on any Azure resource whose
// properties failed to decode.
func (p *AzureProvider) resourceProgram(r spec.Resource, policies spec.Policies) (pulumi.RunFunc, string, error) {
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
		return func(ctx *pulumi.Context) error {
			server, err := declareRelationalDatabase(ctx, r.ID, region, provider.ResourceScope(spec.ProviderAzure, r), *props)
			if err != nil {
				return err
			}
			// The power schedule shares the server's stack, so the
			// existing Destroy path removes both (RFC 012 §4.3).
			return declareDatabaseSchedule(ctx, r.ID, server, rules)
		}, region, nil

	case spec.ResourceTypeComputeInstance:
		props, err := decodeComputeInstanceProperties(r.Properties)
		if err != nil {
			return nil, "", err
		}
		zone, err := compute.Zone(r.Scope.Zones)
		if err != nil {
			return nil, "", fmt.Errorf("azure: resource %q: %w", r.ID, err)
		}
		rules, err := resourceSchedule(r, policies)
		if err != nil {
			return nil, "", err
		}
		return func(ctx *pulumi.Context) error {
			_, err := declareComputeInstance(ctx, r.ID, region, zone, provider.ResourceScope(spec.ProviderAzure, r), *props, rules)
			return err
		}, region, nil

	case spec.ResourceTypeContainerService:
		props, err := decodeContainerServiceProperties(r.Properties, policies.AllowedRegistries)
		if err != nil {
			return nil, "", fmt.Errorf("azure: resource %q: %w", r.ID, err)
		}
		rules, err := resourceSchedule(r, policies)
		if err != nil {
			return nil, "", err
		}
		scope := provider.ResourceScope(spec.ProviderAzure, r)
		return func(ctx *pulumi.Context) error {
			// The Engine provisioned the scope's network, including the
			// delegated Container Apps subnet, before this program runs.
			net, err := lookupScopeNetwork(ctx, scope, containerAppsSubnetName)
			if err != nil {
				return err
			}
			_, err = declareContainerService(ctx, r, scope, net, *props, rules)
			return err
		}, region, nil

	case spec.ResourceTypeBuildPipeline:
		props, err := decodeBuildPipelineProperties(r.Properties)
		if err != nil {
			return nil, "", fmt.Errorf("azure: resource %q: %w", r.ID, err)
		}
		// No lookupScopeNetwork here, unlike the container_service case above:
		// the build runs on ACR's own managed pool, not in the scope's
		// network. It needs the internet to clone a public repository, and
		// the registry it pushes to is reached over ACR's endpoint rather
		// than through the scope. Nothing it produces is reachable from it.
		return func(ctx *pulumi.Context) error {
			_, err := declareBuildPipeline(ctx, r.ID, region, *props, r.Resolved)
			return err
		}, region, nil

	default:
		return nil, "", fmt.Errorf("azure: resource %q: unsupported resource type: %q", r.ID, r.Type)
	}
}

func (p *AzureProvider) Plan(ctx context.Context, r spec.Resource, policies spec.Policies) (provider.Diff, error) {
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
		return provider.Diff{}, fmt.Errorf("azure: preview failed for resource %q: %w", r.ID, err)
	}

	return provider.Diff{
		ResourceID: r.ID,
		Region:     region,
		Action:     pulumiutil.SummarizeChangeSummary(res.ChangeSummary),
		Changes:    pulumiutil.ChangesFromSummary(res.ChangeSummary),
	}, nil
}

func (p *AzureProvider) Apply(ctx context.Context, r spec.Resource, policies spec.Policies) (provider.Result, error) {
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
		return provider.Result{ResourceID: r.ID, Region: region, Status: provider.StatusFailed}, fmt.Errorf("azure: apply failed for resource %q: %w", r.ID, err)
	}
	return provider.Result{
		ResourceID: r.ID,
		Region:     region,
		Status:     provider.StatusApplied,
		Details:    pulumiutil.DetailsFromOutputs(up.Outputs),
	}, nil
}

// Destroy removes the resource from real infrastructure. Validate is
// called first (RFC 011 §1.1B1): without it, a resource with undecodable
// properties reached resourceProgram and panicked.
func (p *AzureProvider) Destroy(ctx context.Context, r spec.Resource, policies spec.Policies) error {
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
		return fmt.Errorf("azure: destroy failed for resource %q: %w", r.ID, err)
	}
	return nil
}
