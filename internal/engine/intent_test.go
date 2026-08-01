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

// TestIntentGate is the regression test for RFC 011 §1.1D3 / §2.4.
//
// Intent was parsed and validated against oneof=deploy update destroy plan
// and then never consulted, so Apply would happily apply a
// destroy-intent Specification. The CLI's destroy command compensated by
// overwriting Intent after translation, which silently masked a
// translator that had misread the user's request.
func TestIntentGate(t *testing.T) {
	tests := []struct {
		name           string
		intent         spec.Intent
		wantApplyErr   bool
		wantDestroyErr bool
	}{
		{name: "deploy", intent: spec.IntentDeploy, wantDestroyErr: true},
		{name: "update", intent: spec.IntentUpdate, wantDestroyErr: true},
		{name: "destroy", intent: spec.IntentDestroy, wantApplyErr: true},
		{name: "plan", intent: spec.IntentPlan, wantApplyErr: true, wantDestroyErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := specWithProvider(spec.ProviderAWS)
			s.Intent = tt.intent

			newEngine := func() *DefaultEngine {
				return New(map[spec.Provider]provider.CloudProvider{
					spec.ProviderAWS: &mockProvider{name: "aws"},
				})
			}

			_, applyErr := newEngine().Apply(context.Background(), s)
			if tt.wantApplyErr {
				if !errors.Is(applyErr, ErrIntentMismatch) {
					t.Errorf("Apply() error = %v, want ErrIntentMismatch for intent %q", applyErr, tt.intent)
				}
			} else if applyErr != nil {
				t.Errorf("Apply() unexpected error for intent %q: %v", tt.intent, applyErr)
			}

			_, destroyErr := newEngine().Destroy(context.Background(), s)
			if tt.wantDestroyErr {
				if !errors.Is(destroyErr, ErrIntentMismatch) {
					t.Errorf("Destroy() error = %v, want ErrIntentMismatch for intent %q", destroyErr, tt.intent)
				}
			} else if destroyErr != nil {
				t.Errorf("Destroy() unexpected error for intent %q: %v", tt.intent, destroyErr)
			}
		})
	}
}

// TestPlanAcceptsAnyIntent: previewing is side-effect free, so it must
// not be gated.
func TestPlanAcceptsAnyIntent(t *testing.T) {
	for _, intent := range []spec.Intent{spec.IntentDeploy, spec.IntentUpdate, spec.IntentDestroy, spec.IntentPlan} {
		t.Run(string(intent), func(t *testing.T) {
			s := specWithProvider(spec.ProviderAWS)
			s.Intent = intent

			e := New(map[spec.Provider]provider.CloudProvider{
				spec.ProviderAWS: &mockProvider{name: "aws"},
			})

			if _, err := e.Plan(context.Background(), s); err != nil {
				t.Errorf("Plan() error = %v, want nil for intent %q", err, intent)
			}
		})
	}
}

// TestIntentGateRunsBeforeProviders verifies the gate short-circuits: a
// mismatched intent must be rejected without touching infrastructure.
func TestIntentGateRunsBeforeProviders(t *testing.T) {
	s := specWithProvider(spec.ProviderAWS)
	s.Intent = spec.IntentDestroy

	m := &mockProvider{name: "aws"}
	e := New(map[spec.Provider]provider.CloudProvider{spec.ProviderAWS: m})

	if _, err := e.Apply(context.Background(), s); !errors.Is(err, ErrIntentMismatch) {
		t.Fatalf("Apply() error = %v, want ErrIntentMismatch", err)
	}
	if m.validateCalls != 0 {
		t.Errorf("provider Validate was called %d times; the intent gate must short-circuit first", m.validateCalls)
	}
}

// TestEngineEnforcesAllowedRegions is the regression test for RFC 011
// §1.1D1 / §2.3: AllowedRegions was enforced only inside the AWS
// provider, so a Specification pinning it deployed anywhere at all on GCP
// and Azure. The Engine now enforces it centrally, so a provider cannot
// silently omit the check.
func TestEngineEnforcesAllowedRegions(t *testing.T) {
	tests := []struct {
		name    string
		region  string
		regions []string
		allowed []string
		wantErr bool
	}{
		{name: "region allowed", region: "eu-central-1", allowed: []string{"eu-central-1"}},
		{name: "region denied", region: "us-east-1", allowed: []string{"eu-central-1"}, wantErr: true},
		{name: "no policy", region: "us-east-1"},
		{name: "unscoped resource is exempt", allowed: []string{"eu-central-1"}},
		{
			name:    "multi-region all allowed",
			regions: []string{"eu-central-1", "eu-west-1"},
			allowed: []string{"eu-central-1", "eu-west-1"},
		},
		{
			name:    "multi-region one denied",
			regions: []string{"eu-central-1", "us-east-1"},
			allowed: []string{"eu-central-1"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := specWithProvider(spec.ProviderAWS)
			s.Resources[0].Scope.Region = tt.region
			s.Resources[0].Scope.Regions = tt.regions
			s.Policies = spec.Policies{AllowedRegions: tt.allowed}

			// A permissive provider: only the Engine's own check can
			// fail this.
			e := New(map[spec.Provider]provider.CloudProvider{
				spec.ProviderAWS: &mockProvider{name: "aws"},
			})

			err := e.Validate(context.Background(), s)

			if tt.wantErr {
				if !errors.Is(err, provider.ErrRegionNotAllowed) {
					t.Fatalf("Validate() error = %v, want it to wrap provider.ErrRegionNotAllowed", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate() unexpected error: %v", err)
			}
		})
	}
}

func TestCheckIntent(t *testing.T) {
	tests := []struct {
		name    string
		intent  spec.Intent
		allowed []spec.Intent
		wantErr bool
	}{
		{name: "single match", intent: spec.IntentDeploy, allowed: []spec.Intent{spec.IntentDeploy}},
		{name: "second match", intent: spec.IntentUpdate, allowed: []spec.Intent{spec.IntentDeploy, spec.IntentUpdate}},
		{name: "no match", intent: spec.IntentDestroy, allowed: []spec.Intent{spec.IntentDeploy}, wantErr: true},
		{name: "empty allowed rejects everything", intent: spec.IntentDeploy, allowed: nil, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := spec.Specification{Intent: tt.intent}

			err := checkIntent(s, tt.allowed...)

			if tt.wantErr {
				if !errors.Is(err, ErrIntentMismatch) {
					t.Fatalf("checkIntent() error = %v, want ErrIntentMismatch", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("checkIntent() unexpected error: %v", err)
			}
		})
	}
}

// destroyRecorder counts Destroy calls so ordering can be asserted.
type destroyRecorder struct {
	mockProvider
	destroyed  []string
	destroyErr error
}

func (d *destroyRecorder) Destroy(ctx context.Context, r spec.Resource, p spec.Policies) error {
	d.destroyed = append(d.destroyed, r.ID)
	return d.destroyErr
}

func TestDestroyProcessesResourcesInReverseOrder(t *testing.T) {
	s := specWithProvider(spec.ProviderAWS)
	s.Intent = spec.IntentDestroy
	s.Resources = append(s.Resources, spec.Resource{
		ID:         "app-cache",
		Type:       spec.ResourceTypeObjectStorage,
		Provider:   spec.ProviderAWS,
		Properties: map[string]any{"bucket_name": "app-cache"},
	})

	rec := &destroyRecorder{mockProvider: mockProvider{name: "aws"}}
	e := New(map[spec.Provider]provider.CloudProvider{spec.ProviderAWS: rec})

	results, err := e.Destroy(context.Background(), s)
	if err != nil {
		t.Fatalf("Destroy() error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("Destroy() returned %d results, want 2", len(results))
	}
	if rec.destroyed[0] != "app-cache" || rec.destroyed[1] != "app-db" {
		t.Errorf("destroy order = %v, want reverse declaration order", rec.destroyed)
	}
	for _, r := range results {
		if r.Status != provider.StatusDestroyed {
			t.Errorf("result status = %q, want %q", r.Status, provider.StatusDestroyed)
		}
	}
}

// TestDestroyReturnsPartialResults is what makes the CLI able to record a
// partially-completed run in the ledger (RFC 011 §1.1G2).
func TestDestroyReturnsPartialResults(t *testing.T) {
	s := specWithProvider(spec.ProviderAWS)
	s.Intent = spec.IntentDestroy

	rec := &destroyRecorder{
		mockProvider: mockProvider{name: "aws"},
		destroyErr:   errors.New("boom"),
	}
	e := New(map[spec.Provider]provider.CloudProvider{spec.ProviderAWS: rec})

	results, err := e.Destroy(context.Background(), s)
	if err == nil {
		t.Fatal("Destroy() error = nil, want the provider failure")
	}
	if results == nil {
		t.Error("Destroy() returned nil results alongside the error; the CLI needs what completed")
	}
}
