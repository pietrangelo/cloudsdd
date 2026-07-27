// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package engine

import (
	"context"
	"errors"
	"testing"

	"cloudsdd/internal/provider"
	"cloudsdd/internal/spec"
)

// mockProvider is a CloudProvider fully controllable by the test caller.
type mockProvider struct {
	name        string
	validateErr error
	plan        provider.Diff
	planErr     error
	apply       provider.Result
	applyErr    error
}

func (m *mockProvider) Name() string { return m.name }

func (m *mockProvider) Validate(ctx context.Context, r spec.Resource, p spec.Policies) error {
	return m.validateErr
}

func (m *mockProvider) Plan(ctx context.Context, r spec.Resource, p spec.Policies) (provider.Diff, error) {
	return m.plan, m.planErr
}

func (m *mockProvider) Apply(ctx context.Context, r spec.Resource, p spec.Policies) (provider.Result, error) {
	return m.apply, m.applyErr
}

func (m *mockProvider) Destroy(ctx context.Context, r spec.Resource, p spec.Policies) error {
	return nil
}

var _ provider.CloudProvider = (*mockProvider)(nil)

func specWithProvider(p spec.Provider) spec.Specification {
	return spec.Specification{
		SDDVersion: "1.0",
		Intent:     spec.IntentDeploy,
		Resources: []spec.Resource{
			{
				ID:         "app-db",
				Type:       spec.ResourceTypeRelationalDatabase,
				Provider:   p,
				Properties: map[string]any{"engine": "postgres"},
			},
		},
	}
}

func TestDefaultEngine_Validate(t *testing.T) {
	tests := []struct {
		name      string
		providers map[spec.Provider]provider.CloudProvider
		spec      spec.Specification
		wantErr   error // if non-nil, checked with errors.Is
		wantAnErr bool
	}{
		{
			name:      "valid spec with registered provider",
			providers: map[spec.Provider]provider.CloudProvider{spec.ProviderAWS: &mockProvider{name: "aws"}},
			spec:      specWithProvider(spec.ProviderAWS),
			wantAnErr: false,
		},
		{
			name:      "unregistered provider",
			providers: map[spec.Provider]provider.CloudProvider{},
			spec:      specWithProvider(spec.ProviderAWS),
			wantErr:   ErrProviderNotFound,
		},
		{
			name:      "agnostic provider resolution not implemented",
			providers: map[spec.Provider]provider.CloudProvider{},
			spec:      specWithProvider(spec.ProviderAgnostic),
			wantErr:   ErrAgnosticResolutionNotImplemented,
		},
		{
			name:      "invalid domain spec rejected before touching providers",
			providers: map[spec.Provider]provider.CloudProvider{},
			spec:      spec.Specification{SDDVersion: "wrong"},
			wantAnErr: true,
		},
		{
			name:      "provider-level validation error propagated",
			providers: map[spec.Provider]provider.CloudProvider{spec.ProviderAWS: &mockProvider{name: "aws", validateErr: errors.New("boom")}},
			spec:      specWithProvider(spec.ProviderAWS),
			wantAnErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := New(tt.providers)
			err := e.Validate(context.Background(), tt.spec)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Validate() error = %v, want errors.Is %v", err, tt.wantErr)
				}
				return
			}
			if (err != nil) != tt.wantAnErr {
				t.Fatalf("Validate() error = %v, wantAnErr %v", err, tt.wantAnErr)
			}
		})
	}
}

func TestDefaultEngine_Plan(t *testing.T) {
	wantDiff := provider.Diff{ResourceID: "app-db", Action: provider.ActionCreate}

	e := New(map[spec.Provider]provider.CloudProvider{
		spec.ProviderAWS: &mockProvider{name: "aws", plan: wantDiff},
	})

	diffs, err := e.Plan(context.Background(), specWithProvider(spec.ProviderAWS))
	if err != nil {
		t.Fatalf("Plan() unexpected error: %v", err)
	}
	if len(diffs) != 1 || diffs[0].ResourceID != wantDiff.ResourceID || diffs[0].Action != wantDiff.Action {
		t.Fatalf("Plan() = %+v, want [%+v]", diffs, wantDiff)
	}
}

func TestDefaultEngine_Plan_ProviderError(t *testing.T) {
	e := New(map[spec.Provider]provider.CloudProvider{
		spec.ProviderAWS: &mockProvider{name: "aws", planErr: errors.New("boom")},
	})

	if _, err := e.Plan(context.Background(), specWithProvider(spec.ProviderAWS)); err == nil {
		t.Fatal("Plan() expected error, got nil")
	}
}

func TestDefaultEngine_Apply(t *testing.T) {
	wantResult := provider.Result{ResourceID: "app-db", Status: provider.StatusApplied}

	e := New(map[spec.Provider]provider.CloudProvider{
		spec.ProviderAWS: &mockProvider{name: "aws", apply: wantResult},
	})

	results, err := e.Apply(context.Background(), specWithProvider(spec.ProviderAWS))
	if err != nil {
		t.Fatalf("Apply() unexpected error: %v", err)
	}
	if len(results) != 1 || results[0].ResourceID != wantResult.ResourceID || results[0].Status != wantResult.Status {
		t.Fatalf("Apply() = %+v, want [%+v]", results, wantResult)
	}
}

func TestDefaultEngine_Apply_ProviderError(t *testing.T) {
	e := New(map[spec.Provider]provider.CloudProvider{
		spec.ProviderAWS: &mockProvider{name: "aws", applyErr: errors.New("boom")},
	})

	if _, err := e.Apply(context.Background(), specWithProvider(spec.ProviderAWS)); err == nil {
		t.Fatal("Apply() expected error, got nil")
	}
}

func specWithAccount(account string) spec.Specification {
	s := specWithProvider(spec.ProviderAWS)
	s.Resources[0].Account = account
	return s
}

func TestDefaultEngine_ResolveTargetProvider(t *testing.T) {
	targetProvider := &mockProvider{name: "aws-target"}
	factoryCalls := 0
	factory := func(ctx context.Context, target DeploymentTarget) (provider.CloudProvider, error) {
		factoryCalls++
		return targetProvider, nil
	}

	tests := []struct {
		name    string
		targets map[string]DeploymentTarget
		opts    []Option
		account string
		wantErr error
	}{
		{
			name:    "account references unknown target",
			targets: map[string]DeploymentTarget{},
			account: "prod",
			wantErr: ErrDeploymentTargetNotFound,
		},
		{
			name: "disabled target rejected without contacting factory",
			targets: map[string]DeploymentTarget{
				"prod": {Name: "prod", Provider: spec.ProviderAWS, Enabled: false},
			},
			account: "prod",
			wantErr: ErrDeploymentTargetDisabled,
		},
		{
			name: "provider mismatch rejected",
			targets: map[string]DeploymentTarget{
				"prod": {Name: "prod", Provider: spec.ProviderGCP, Enabled: true},
			},
			account: "prod",
			wantErr: ErrDeploymentTargetProviderMismatch,
		},
		{
			name: "enabled target without registered factory",
			targets: map[string]DeploymentTarget{
				"prod": {Name: "prod", Provider: spec.ProviderAWS, Enabled: true},
			},
			account: "prod",
			wantErr: ErrNoTargetProviderFactory,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := New(map[spec.Provider]provider.CloudProvider{}, WithDeploymentTargets(tt.targets))
			err := e.Validate(context.Background(), specWithAccount(tt.account))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() error = %v, want errors.Is %v", err, tt.wantErr)
			}
		})
	}

	t.Run("enabled target with factory is used and cached", func(t *testing.T) {
		e := New(
			map[spec.Provider]provider.CloudProvider{},
			WithDeploymentTargets(map[string]DeploymentTarget{
				"prod": {Name: "prod", Provider: spec.ProviderAWS, Enabled: true},
			}),
			WithTargetProviderFactory(spec.ProviderAWS, factory),
		)

		s := specWithAccount("prod")
		if err := e.Validate(context.Background(), s); err != nil {
			t.Fatalf("Validate() unexpected error: %v", err)
		}
		if _, err := e.Plan(context.Background(), s); err != nil {
			t.Fatalf("Plan() unexpected error: %v", err)
		}
		if factoryCalls != 1 {
			t.Fatalf("factory calls = %d, want 1 (target provider should be cached)", factoryCalls)
		}
	})

	t.Run("resource without account is unaffected", func(t *testing.T) {
		e := New(map[spec.Provider]provider.CloudProvider{spec.ProviderAWS: &mockProvider{name: "aws"}})
		if err := e.Validate(context.Background(), specWithProvider(spec.ProviderAWS)); err != nil {
			t.Fatalf("Validate() unexpected error: %v", err)
		}
	})
}

func TestNew_RegistryIsolation(t *testing.T) {
	providers := map[spec.Provider]provider.CloudProvider{
		spec.ProviderAWS: &mockProvider{name: "aws"},
	}
	e := New(providers)

	providers[spec.ProviderGCP] = &mockProvider{name: "gcp"}

	err := e.Validate(context.Background(), specWithProvider(spec.ProviderGCP))
	if !errors.Is(err, ErrProviderNotFound) {
		t.Fatalf("expected engine registry to be isolated from caller map mutation, got err = %v", err)
	}
}
