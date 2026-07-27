package engine

import (
	"context"
	"errors"
	"testing"

	"cloudsdd/internal/provider"
	"cloudsdd/internal/spec"
)

// mockProvider è un CloudProvider di test interamente controllabile dal chiamante.
type mockProvider struct {
	name        string
	validateErr error
	plan        provider.Diff
	planErr     error
	apply       provider.Result
	applyErr    error
}

func (m *mockProvider) Name() string { return m.name }

func (m *mockProvider) Validate(ctx context.Context, r spec.Resource) error {
	return m.validateErr
}

func (m *mockProvider) Plan(ctx context.Context, r spec.Resource) (provider.Diff, error) {
	return m.plan, m.planErr
}

func (m *mockProvider) Apply(ctx context.Context, r spec.Resource) (provider.Result, error) {
	return m.apply, m.applyErr
}

func (m *mockProvider) Destroy(ctx context.Context, r spec.Resource) error { return nil }

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
		wantErr   error // se non nil, verificato con errors.Is
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
