// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"cloudsdd/internal/config"
	"cloudsdd/internal/provider"
	"cloudsdd/internal/spec"
)

// regionalProvider accepts only its own region dialect, the way the real
// providers do (RFC 014 §2.3).
type regionalProvider struct {
	fakeProvider
	regions map[string]bool
}

func (p *regionalProvider) Validate(ctx context.Context, r spec.Resource, _ spec.Policies) error {
	if !p.regions[r.Scope.Region] {
		return errors.New(p.name + ": region not supported")
	}
	return nil
}

// agnosticSpec is a single bucket with no provider named.
func agnosticSpec(region string) spec.Specification {
	return spec.Specification{
		SDDVersion: "1.0",
		Intent:     spec.IntentDeploy,
		Resources: []spec.Resource{{
			ID:         "assets",
			Type:       spec.ResourceTypeObjectStorage,
			Provider:   spec.ProviderAgnostic,
			Scope:      spec.Scope{Environment: "dev", Region: region},
			Properties: map[string]any{"bucket_name": "assets"},
		}},
	}
}

// useRegionalProviders swaps the factories for providers that accept one
// region each, and returns the map recording which were constructed.
func useRegionalProviders(t *testing.T, fail map[spec.Provider]error) map[spec.Provider]bool {
	t.Helper()

	orig := providerFactories
	t.Cleanup(func() { providerFactories = orig })

	built := map[spec.Provider]bool{}
	regions := map[spec.Provider]string{
		spec.ProviderAWS:   "eu-central-1",
		spec.ProviderGCP:   "europe-west1",
		spec.ProviderAzure: "westeurope",
	}

	providerFactories = map[spec.Provider]func() (provider.CloudProvider, error){}
	for name, region := range regions {
		name, region := name, region
		providerFactories[name] = func() (provider.CloudProvider, error) {
			if err, ok := fail[name]; ok {
				return nil, err
			}
			built[name] = true
			return &regionalProvider{
				fakeProvider: fakeProvider{name: string(name)},
				regions:      map[string]bool{region: true},
			}, nil
		}
	}
	return built
}

func TestRunIntentResolvesAgnosticByRegion(t *testing.T) {
	resetScheduleFlags(t)
	h := newHarness(t, agnosticSpec("europe-west1"), nil, "y\n")
	useRegionalProviders(t, nil)

	if err := runIntent(h.cmd, []string{"a", "bucket"}, spec.IntentDeploy); err != nil {
		t.Fatalf("runIntent() error: %v", err)
	}

	got := h.out.String()
	// The rendered Specification must name a concrete cloud: the user
	// approves what will actually be built, never "agnostic".
	if strings.Contains(got, `"provider": "agnostic"`) {
		t.Errorf("the approved specification still says agnostic:\n%s", got)
	}
	if !strings.Contains(got, `"provider": "gcp"`) {
		t.Errorf("the specification does not name the resolved provider:\n%s", got)
	}
	// And the decision made on the user's behalf is explained.
	if !strings.Contains(got, "Resolved agnostic resources:") ||
		!strings.Contains(got, "assets -> gcp") {
		t.Errorf("the resolution was not reported:\n%s", got)
	}
}

func TestRunIntentReportsProvidersExcludedFromCandidacy(t *testing.T) {
	resetScheduleFlags(t)
	h := newHarness(t, agnosticSpec("eu-central-1"), nil, "y\n")
	// Azure cannot be constructed here. It must be reported, not silently
	// dropped: otherwise the same specification resolves differently on a
	// colleague's machine with no way to see why (RFC 014 §2.5).
	useRegionalProviders(t, map[spec.Provider]error{
		spec.ProviderAzure: errors.New("CLOUDSDD_PULUMI_PASSPHRASE must be set"),
	})

	if err := runIntent(h.cmd, []string{"a", "bucket"}, spec.IntentDeploy); err != nil {
		t.Fatalf("runIntent() error: %v", err)
	}

	got := h.out.String()
	if !strings.Contains(got, "azure is not available as a candidate") {
		t.Errorf("the excluded provider was not reported:\n%s", got)
	}
	if !strings.Contains(got, "CLOUDSDD_PULUMI_PASSPHRASE") {
		t.Errorf("the exclusion does not say why:\n%s", got)
	}
	if !strings.Contains(got, "assets -> aws") {
		t.Errorf("resolution did not fall to the remaining candidate:\n%s", got)
	}
}

func TestRunIntentFailsHardOnAnExplicitlyNamedProvider(t *testing.T) {
	resetScheduleFlags(t)

	s := agnosticSpec("eu-central-1")
	s.Resources[0].Provider = spec.ProviderAWS
	h := newHarness(t, s, nil, "y\n")
	// Unlike a candidate, a provider the user named by hand must be a
	// hard failure: they asked for it specifically.
	useRegionalProviders(t, map[spec.Provider]error{
		spec.ProviderAWS: errors.New("CLOUDSDD_PULUMI_PASSPHRASE must be set"),
	})

	err := runIntent(h.cmd, []string{"a", "bucket"}, spec.IntentDeploy)
	if err == nil {
		t.Fatal("runIntent() error = nil, want the factory failure")
	}
	if !strings.Contains(err.Error(), "failed to initialize aws provider") {
		t.Errorf("error = %q, want it to name the provider the user asked for", err)
	}
}

func TestRunIntentRefusesAnAmbiguousResolution(t *testing.T) {
	resetScheduleFlags(t)
	h := newHarness(t, agnosticSpec("shared"), nil, "y\n")

	orig := providerFactories
	t.Cleanup(func() { providerFactories = orig })

	// Two clouds accept the same region and nothing says which to use.
	providerFactories = map[spec.Provider]func() (provider.CloudProvider, error){}
	for _, name := range []spec.Provider{spec.ProviderAWS, spec.ProviderGCP} {
		name := name
		providerFactories[name] = func() (provider.CloudProvider, error) {
			return &regionalProvider{
				fakeProvider: fakeProvider{name: string(name)},
				regions:      map[string]bool{"shared": true},
			}, nil
		}
	}

	err := runIntent(h.cmd, []string{"a", "bucket"}, spec.IntentDeploy)
	if err == nil {
		t.Fatal("runIntent() error = nil, want a refusal to choose")
	}
	if !strings.Contains(err.Error(), "provider_preference") {
		t.Errorf("error = %q, want it to say how to resolve the ambiguity", err)
	}
	// Nothing may be applied when the target cloud is undecided.
	if strings.Contains(h.out.String(), "Apply completed") {
		t.Errorf("resources were applied despite an unresolved provider:\n%s", h.out.String())
	}
}

func TestBuildEngineOnlyAttemptsEveryProviderWhenNeeded(t *testing.T) {
	tests := []struct {
		name       string
		provider   spec.Provider
		wantBuilt  []spec.Provider
		wantNotAll bool
	}{
		{
			// An explicit provider keeps RFC 011 §2.8's laziness: no
			// reason to demand credentials for clouds nobody named.
			name:       "explicit provider builds only itself",
			provider:   spec.ProviderGCP,
			wantBuilt:  []spec.Provider{spec.ProviderGCP},
			wantNotAll: true,
		},
		{
			name:      "agnostic needs candidates",
			provider:  spec.ProviderAgnostic,
			wantBuilt: []spec.Provider{spec.ProviderAWS, spec.ProviderGCP, spec.ProviderAzure},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(config.HomeEnv, t.TempDir())
			built := useRegionalProviders(t, nil)

			s := agnosticSpec("europe-west1")
			s.Resources[0].Provider = tt.provider

			if _, _, err := buildEngine(s, nil); err != nil {
				t.Fatalf("buildEngine() error: %v", err)
			}

			for _, want := range tt.wantBuilt {
				if !built[want] {
					t.Errorf("provider %q was not constructed", want)
				}
			}
			if tt.wantNotAll && len(built) != len(tt.wantBuilt) {
				t.Errorf("constructed %v, want only %v", built, tt.wantBuilt)
			}
		})
	}
}

func TestBuildEnginePassesTheConfiguredDefault(t *testing.T) {
	t.Setenv(config.HomeEnv, t.TempDir())
	useRegionalProviders(t, nil)

	orig := providerFactories
	t.Cleanup(func() { providerFactories = orig })
	providerFactories = map[spec.Provider]func() (provider.CloudProvider, error){}
	for _, name := range []spec.Provider{spec.ProviderAWS, spec.ProviderGCP} {
		name := name
		providerFactories[name] = func() (provider.CloudProvider, error) {
			return &regionalProvider{
				fakeProvider: fakeProvider{name: string(name)},
				regions:      map[string]bool{"shared": true},
			}, nil
		}
	}

	cfg := &config.Config{Defaults: config.DefaultsConfig{Provider: "gcp"}}
	eng, _, err := buildEngine(agnosticSpec("shared"), cfg)
	if err != nil {
		t.Fatalf("buildEngine() error: %v", err)
	}

	// Ambiguous on its own; the configured default is what settles it.
	resolved, resolutions, err := eng.Resolve(context.Background(), agnosticSpec("shared"))
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got := resolved.Resources[0].Provider; got != spec.ProviderGCP {
		t.Errorf("provider = %q, want the configured default", got)
	}
	if !strings.Contains(resolutions[0].Reason, "configured default") {
		t.Errorf("reason = %q, want it to credit the configuration", resolutions[0].Reason)
	}
}
