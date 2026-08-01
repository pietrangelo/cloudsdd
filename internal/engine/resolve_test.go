// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"cloudsdd/internal/provider"
	"cloudsdd/internal/spec"
)

// pickyProvider accepts only the regions it was told to, which is how the
// real providers behave: their region formats are mutually exclusive, so
// the region a user already wrote usually settles the choice on its own
// (RFC 014 §2.3).
type pickyProvider struct {
	name    string
	regions map[string]bool
	// rejectAll makes the provider refuse everything, standing in for a
	// resource type it does not implement.
	rejectAll bool

	// operated records the operations that reached this provider, so a
	// test can assert which cloud an agnostic resource was actually sent
	// to rather than only that the call did not fail.
	operated []string
}

func (p *pickyProvider) Name() string { return p.name }

func (p *pickyProvider) Validate(ctx context.Context, r spec.Resource, _ spec.Policies) error {
	if p.rejectAll {
		return fmt.Errorf("%s: unsupported resource type: %q", p.name, r.Type)
	}
	if !p.regions[r.Scope.Region] {
		return fmt.Errorf("%s: %q is not a valid region", p.name, r.Scope.Region)
	}
	return nil
}

func (p *pickyProvider) Plan(ctx context.Context, r spec.Resource, _ spec.Policies) (provider.Diff, error) {
	p.operated = append(p.operated, "plan")
	return provider.Diff{ResourceID: r.ID, Action: provider.ActionCreate}, nil
}

func (p *pickyProvider) Apply(ctx context.Context, r spec.Resource, _ spec.Policies) (provider.Result, error) {
	p.operated = append(p.operated, "apply")
	return provider.Result{ResourceID: r.ID, Status: provider.StatusApplied}, nil
}

func (p *pickyProvider) Destroy(ctx context.Context, r spec.Resource, _ spec.Policies) error {
	p.operated = append(p.operated, "destroy")
	return nil
}

func (p *pickyProvider) EnsureNetwork(ctx context.Context, s provider.NetworkScope, _ spec.Policies) error {
	p.operated = append(p.operated, "ensure-network")
	return nil
}

func (p *pickyProvider) DestroyNetwork(ctx context.Context, s provider.NetworkScope, _ spec.Policies) error {
	p.operated = append(p.operated, "destroy-network")
	return nil
}

var _ provider.CloudProvider = (*pickyProvider)(nil)

// threeClouds mirrors the real registry: each provider accepts only its
// own region dialect.
func threeClouds() map[spec.Provider]provider.CloudProvider {
	return map[spec.Provider]provider.CloudProvider{
		spec.ProviderAWS:   &pickyProvider{name: "aws", regions: map[string]bool{"eu-central-1": true}},
		spec.ProviderGCP:   &pickyProvider{name: "gcp", regions: map[string]bool{"europe-west1": true}},
		spec.ProviderAzure: &pickyProvider{name: "azure", regions: map[string]bool{"westeurope": true}},
	}
}

func agnosticSpec(region string, mutate func(*spec.Specification)) spec.Specification {
	s := spec.Specification{
		SDDVersion: "1.0",
		Intent:     spec.IntentDeploy,
		Resources: []spec.Resource{{
			ID:         "app-db",
			Type:       spec.ResourceTypeRelationalDatabase,
			Provider:   spec.ProviderAgnostic,
			Scope:      spec.Scope{Region: region},
			Properties: map[string]any{"engine": "postgres"},
		}},
	}
	if mutate != nil {
		mutate(&s)
	}
	return s
}

func TestResolveByRegionFormat(t *testing.T) {
	// The headline case: a region the user already wrote leaves exactly
	// one candidate, so nothing has to be configured at all.
	tests := []struct {
		region string
		want   spec.Provider
	}{
		{region: "eu-central-1", want: spec.ProviderAWS},
		{region: "europe-west1", want: spec.ProviderGCP},
		{region: "westeurope", want: spec.ProviderAzure},
	}

	for _, tt := range tests {
		t.Run(tt.region, func(t *testing.T) {
			e := New(threeClouds())

			resolved, resolutions, err := e.Resolve(context.Background(), agnosticSpec(tt.region, nil))
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if got := resolved.Resources[0].Provider; got != tt.want {
				t.Errorf("provider = %q, want %q", got, tt.want)
			}
			if len(resolutions) != 1 {
				t.Fatalf("resolutions = %v, want one", resolutions)
			}
			if !strings.Contains(resolutions[0].Reason, "only provider") {
				t.Errorf("reason = %q, want it to say the choice was forced", resolutions[0].Reason)
			}
		})
	}
}

func TestResolveDoesNotMutateTheInput(t *testing.T) {
	// The caller keeps its own Specification: the CLI prints the resolved
	// copy, and nothing should rewrite the value it was handed.
	e := New(threeClouds())
	original := agnosticSpec("eu-central-1", nil)

	if _, _, err := e.Resolve(context.Background(), original); err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got := original.Resources[0].Provider; got != spec.ProviderAgnostic {
		t.Errorf("the caller's specification was rewritten to %q", got)
	}
}

func TestResolveAmbiguityIsRefused(t *testing.T) {
	// Two providers accept the same region, and nothing says which.
	e := New(map[spec.Provider]provider.CloudProvider{
		spec.ProviderAWS: &pickyProvider{name: "aws", regions: map[string]bool{"shared": true}},
		spec.ProviderGCP: &pickyProvider{name: "gcp", regions: map[string]bool{"shared": true}},
	})

	_, _, err := e.Resolve(context.Background(), agnosticSpec("shared", nil))
	if !errors.Is(err, ErrAmbiguousProvider) {
		t.Fatalf("Resolve() error = %v, want %v", err, ErrAmbiguousProvider)
	}
	// The message has to name both the candidates and the ways out, or
	// the user is stuck.
	for _, want := range []string{"aws", "gcp", "provider_preference", "defaults.provider"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

func TestResolvePreferenceOrder(t *testing.T) {
	both := map[spec.Provider]provider.CloudProvider{
		spec.ProviderAWS:   &pickyProvider{name: "aws", regions: map[string]bool{"shared": true}},
		spec.ProviderGCP:   &pickyProvider{name: "gcp", regions: map[string]bool{"shared": true}},
		spec.ProviderAzure: &pickyProvider{name: "azure", regions: map[string]bool{"other": true}},
	}

	tests := []struct {
		name       string
		preference []spec.Provider
		configured spec.Provider
		want       spec.Provider
		wantReason string
	}{
		{
			name:       "specification preference wins",
			preference: []spec.Provider{spec.ProviderGCP, spec.ProviderAWS},
			want:       spec.ProviderGCP,
			wantReason: "provider_preference",
		},
		{
			name:       "order within the preference matters",
			preference: []spec.Provider{spec.ProviderAWS, spec.ProviderGCP},
			want:       spec.ProviderAWS,
			wantReason: "provider_preference",
		},
		{
			// A preference naming only providers that cannot take the
			// resource must fall through, not fail.
			name:       "unusable preference falls through to the default",
			preference: []spec.Provider{spec.ProviderAzure},
			configured: spec.ProviderGCP,
			want:       spec.ProviderGCP,
			wantReason: "configured default",
		},
		{
			name:       "config default applies when the spec is silent",
			configured: spec.ProviderAWS,
			want:       spec.ProviderAWS,
			wantReason: "configured default",
		},
		{
			// The specification outranks the machine's configuration:
			// what the user wrote beats what their laptop prefers.
			name:       "specification outranks configuration",
			preference: []spec.Provider{spec.ProviderGCP},
			configured: spec.ProviderAWS,
			want:       spec.ProviderGCP,
			wantReason: "provider_preference",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var opts []Option
			if tt.configured != "" {
				opts = append(opts, WithDefaultProvider(tt.configured))
			}
			e := New(both, opts...)

			s := agnosticSpec("shared", func(s *spec.Specification) {
				s.Policies.ProviderPreference = tt.preference
			})

			resolved, resolutions, err := e.Resolve(context.Background(), s)
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if got := resolved.Resources[0].Provider; got != tt.want {
				t.Errorf("provider = %q, want %q", got, tt.want)
			}
			if !strings.Contains(resolutions[0].Reason, tt.wantReason) {
				t.Errorf("reason = %q, want it to mention %q", resolutions[0].Reason, tt.wantReason)
			}
		})
	}
}

func TestResolveNoCandidateReportsEveryReason(t *testing.T) {
	// A user whose resource nobody can take learns more from three
	// specific complaints than from one summary.
	e := New(map[spec.Provider]provider.CloudProvider{
		spec.ProviderAWS:   &pickyProvider{name: "aws", rejectAll: true},
		spec.ProviderGCP:   &pickyProvider{name: "gcp", rejectAll: true},
		spec.ProviderAzure: &pickyProvider{name: "azure", rejectAll: true},
	})

	_, _, err := e.Resolve(context.Background(), agnosticSpec("eu-central-1", nil))
	if !errors.Is(err, ErrNoCandidateProvider) {
		t.Fatalf("Resolve() error = %v, want %v", err, ErrNoCandidateProvider)
	}
	for _, want := range []string{"aws:", "gcp:", "azure:", "unsupported resource type"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
}

func TestResolveIsDeterministic(t *testing.T) {
	// DefaultEngine.providers is a Go map and ranging over one is
	// randomised. Without a sorted candidate order this test fails
	// intermittently — and in production the same Specification would
	// reach a different cloud on different runs.
	e := New(map[spec.Provider]provider.CloudProvider{
		spec.ProviderAWS:   &pickyProvider{name: "aws", regions: map[string]bool{"shared": true}},
		spec.ProviderGCP:   &pickyProvider{name: "gcp", regions: map[string]bool{"shared": true}},
		spec.ProviderAzure: &pickyProvider{name: "azure", regions: map[string]bool{"shared": true}},
	})
	s := agnosticSpec("shared", func(s *spec.Specification) {
		s.Policies.ProviderPreference = []spec.Provider{spec.ProviderAzure}
	})

	first, _, err := e.Resolve(context.Background(), s)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	for i := 0; i < 50; i++ {
		got, _, err := e.Resolve(context.Background(), s)
		if err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
		if got.Resources[0].Provider != first.Resources[0].Provider {
			t.Fatalf("run %d resolved to %q, first run resolved to %q",
				i, got.Resources[0].Provider, first.Resources[0].Provider)
		}
	}
}

func TestResolveLeavesConcreteResourcesAlone(t *testing.T) {
	e := New(threeClouds())

	s := agnosticSpec("europe-west1", func(s *spec.Specification) {
		s.Resources[0].Provider = spec.ProviderGCP
	})

	resolved, resolutions, err := e.Resolve(context.Background(), s)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got := resolved.Resources[0].Provider; got != spec.ProviderGCP {
		t.Errorf("provider = %q, want it untouched", got)
	}
	if len(resolutions) != 0 {
		t.Errorf("resolutions = %v, want none for an explicitly named provider", resolutions)
	}
}

func TestResolveLeavesAccountResourcesToTheTarget(t *testing.T) {
	// An Account already names its provider through the DeploymentTarget
	// it references (RFC 004 §2.2); resolution must not step on it.
	e := New(threeClouds())

	s := agnosticSpec("eu-central-1", func(s *spec.Specification) {
		s.Resources[0].Account = "prod-account"
	})

	resolved, resolutions, err := e.Resolve(context.Background(), s)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got := resolved.Resources[0].Provider; got != spec.ProviderAgnostic {
		t.Errorf("provider = %q, want the DeploymentTarget path to own it", got)
	}
	if len(resolutions) != 0 {
		t.Errorf("resolutions = %v, want none", resolutions)
	}
}

func TestResolveMultiRegionRequiresOneProviderForAll(t *testing.T) {
	// A provider that can express only some of a resource's regions is
	// not a candidate: picking it would fail halfway through an apply.
	e := New(map[spec.Provider]provider.CloudProvider{
		spec.ProviderAWS: &pickyProvider{name: "aws", regions: map[string]bool{
			"eu-central-1": true, "eu-west-1": true,
		}},
		spec.ProviderGCP: &pickyProvider{name: "gcp", regions: map[string]bool{
			"eu-central-1": true,
		}},
	})

	s := agnosticSpec("", func(s *spec.Specification) {
		s.Resources[0].Scope.Regions = []string{"eu-central-1", "eu-west-1"}
	})

	resolved, _, err := e.Resolve(context.Background(), s)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got := resolved.Resources[0].Provider; got != spec.ProviderAWS {
		t.Errorf("provider = %q, want the only one that covers every region", got)
	}
}

func TestValidateResolvesBeforeDelegating(t *testing.T) {
	// An Engine driven directly, without the CLI's explicit Resolve,
	// must still never hand a provider an agnostic resource.
	e := New(threeClouds())

	if err := e.Validate(context.Background(), agnosticSpec("westeurope", nil)); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

// TestOperationsResolveAgnostic guards a defect that Validate alone could
// not surface: Validate resolves a *copy* of the Specification, so an
// operation that validated and then walked its own resources still held
// "agnostic" and failed on a registry lookup that can never succeed. The
// CLI resolves explicitly before planning and so never saw it; an Engine
// driven directly failed on every operation.
func TestOperationsResolveAgnostic(t *testing.T) {
	tests := []struct {
		name   string
		intent spec.Intent
		run    func(*DefaultEngine, spec.Specification) error
		// want is the full sequence the chosen provider must observe.
		// Apply is preceded by EnsureNetwork (RFC 016 §2.2); Plan is
		// not, because Plan makes no changes and creating a VPC to
		// preview a database would be a change.
		want []string
	}{
		{
			name:   "plan",
			intent: spec.IntentDeploy,
			run: func(e *DefaultEngine, s spec.Specification) error {
				diffs, err := e.Plan(context.Background(), s)
				if err == nil && len(diffs) != 1 {
					return fmt.Errorf("got %d diffs, want 1", len(diffs))
				}
				return err
			},
			want: []string{"plan"},
		},
		{
			name:   "apply",
			intent: spec.IntentDeploy,
			run: func(e *DefaultEngine, s spec.Specification) error {
				results, err := e.Apply(context.Background(), s)
				if err == nil && len(results) != 1 {
					return fmt.Errorf("got %d results, want 1", len(results))
				}
				return err
			},
			want: []string{"ensure-network", "apply"},
		},
		{
			name:   "destroy",
			intent: spec.IntentDestroy,
			run: func(e *DefaultEngine, s spec.Specification) error {
				results, err := e.Destroy(context.Background(), s)
				if err == nil && len(results) != 1 {
					return fmt.Errorf("got %d results, want 1", len(results))
				}
				return err
			},
			want: []string{"destroy"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := threeClouds()
			e := New(registry)

			s := agnosticSpec("europe-west1", func(s *spec.Specification) {
				s.Intent = tt.intent
			})
			if err := tt.run(e, s); err != nil {
				t.Fatalf("%s error = %v", tt.name, err)
			}

			// The region is GCP's, so GCP is the only candidate: the
			// operation must have reached it and nothing else.
			gcp := registry[spec.ProviderGCP].(*pickyProvider)
			if !slices.Equal(gcp.operated, tt.want) {
				t.Errorf("gcp received %v, want %v", gcp.operated, tt.want)
			}
			for _, name := range []spec.Provider{spec.ProviderAWS, spec.ProviderAzure} {
				if got := registry[name].(*pickyProvider).operated; len(got) != 0 {
					t.Errorf("%s received %v, want no operation", name, got)
				}
			}
		})
	}
}
