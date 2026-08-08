// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package engine

import (
	"context"
	"errors"
	"testing"

	"cloudsdd/internal/provider"
	"cloudsdd/internal/provider/pipeline"
	"cloudsdd/internal/spec"
)

// TestResolveBuilds covers RFC 018 §2.4.1: the revision becomes a commit
// before anything is applied, and the service that consumes the pipeline is
// handed the same answer.
func TestResolveBuilds(t *testing.T) {
	engineWith := func(resolver pipeline.RevisionResolver) *DefaultEngine {
		return New(map[spec.Provider]provider.CloudProvider{
			spec.ProviderAWS: &mockProvider{name: "aws"},
		}, WithRevisionResolver(resolver))
	}

	t.Run("a pipeline and the service that consumes it", func(t *testing.T) {
		resolver := &stubResolver{commit: testCommit}
		s := graphSpec(
			serviceResource("api", spec.ProviderAWS, 8080, "api-build"),
			pipelineResource("api-build", spec.ProviderAWS, 8080),
		)

		got, err := engineWith(resolver).resolveBuilds(context.Background(), s)
		if err != nil {
			t.Fatalf("resolveBuilds() = %v, want nil", err)
		}

		for _, r := range got.Resources {
			if r.Resolved == nil {
				t.Fatalf("resource %q was not resolved", r.ID)
			}
			// Both sides get the same two facts: what is published, and
			// under which tag.
			if r.Resolved.Commit != testCommit {
				t.Errorf("resource %q resolved to %q, want %q", r.ID, r.Resolved.Commit, testCommit)
			}
			if r.Resolved.ImageName != "acme/api" {
				t.Errorf("resource %q resolved image name %q, want %q", r.ID, r.Resolved.ImageName, "acme/api")
			}
		}

		// One lookup, not one per resource that needs the answer.
		if len(resolver.calls) != 1 {
			t.Errorf("resolved %d times, want once: %v", len(resolver.calls), resolver.calls)
		}
		if want := "https://github.com/acme/api@main"; resolver.calls[0] != want {
			t.Errorf("resolved %q, want %q", resolver.calls[0], want)
		}

		// The input must not have been mutated: callers pass a
		// Specification by value and are entitled to keep theirs.
		if s.Resources[0].Resolved != nil {
			t.Error("resolveBuilds mutated the Specification it was given")
		}
	})

	t.Run("a Specification with no pipeline touches no network", func(t *testing.T) {
		resolver := &stubResolver{err: errors.New("no lookup should have happened")}
		s := graphSpec(
			serviceResource("api", spec.ProviderAWS, 8080, ""),
			databaseResource("app-db", spec.ProviderAWS),
		)

		got, err := engineWith(resolver).resolveBuilds(context.Background(), s)
		if err != nil {
			t.Fatalf("resolveBuilds() = %v, want nil", err)
		}
		if len(resolver.calls) != 0 {
			t.Errorf("resolved %v, want nothing", resolver.calls)
		}
		for _, r := range got.Resources {
			if r.Resolved != nil {
				t.Errorf("resource %q was resolved and has no build", r.ID)
			}
		}
	})

	t.Run("two services on one pipeline share one answer", func(t *testing.T) {
		resolver := &stubResolver{commit: testCommit}
		s := graphSpec(
			pipelineResource("api-build", spec.ProviderAWS, 8080, 9090),
			serviceResource("api", spec.ProviderAWS, 8080, "api-build"),
			serviceResource("metrics", spec.ProviderAWS, 9090, "api-build"),
		)

		got, err := engineWith(resolver).resolveBuilds(context.Background(), s)
		if err != nil {
			t.Fatalf("resolveBuilds() = %v, want nil", err)
		}
		if len(resolver.calls) != 1 {
			t.Errorf("resolved %d times, want once", len(resolver.calls))
		}
		// Two services deployed from one build must run one image; a
		// second lookup could answer differently if the branch moved
		// between them.
		if got.Resources[1].Resolved != got.Resources[2].Resolved {
			t.Error("the two services were given different answers for one pipeline")
		}
	})

	t.Run("an unresolvable revision stops the Specification", func(t *testing.T) {
		resolver := &stubResolver{err: pipeline.ErrRevisionNotFound}
		s := graphSpec(pipelineResource("api-build", spec.ProviderAWS, 8080))

		_, err := engineWith(resolver).resolveBuilds(context.Background(), s)
		if !errors.Is(err, pipeline.ErrRevisionNotFound) {
			t.Fatalf("resolveBuilds() = %v, want %v", err, pipeline.ErrRevisionNotFound)
		}
	})
}

// TestResolveBuildsRefusesAnIncompleteSource: resolving nothing would mean
// building whatever the branch points at when the build eventually runs,
// which is the "deploy whatever is newest" §2.4 exists to refuse.
func TestResolveBuildsRefusesAnIncompleteSource(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(map[string]any)
		wantErr error
	}{
		{
			name:    "no source at all",
			mutate:  func(p map[string]any) { delete(p, "source") },
			wantErr: ErrIncompleteBuildSource,
		},
		{
			name:    "no revision",
			mutate:  func(p map[string]any) { p["source"] = map[string]any{"repository": "https://github.com/acme/api"} },
			wantErr: ErrIncompleteBuildSource,
		},
		{
			name:    "no image name",
			mutate:  func(p map[string]any) { delete(p, "image_name") },
			wantErr: ErrIncompleteBuildSource,
		},
		{
			// The shared rules hold here too, before anything is sent over
			// the network.
			name: "an http repository",
			mutate: func(p map[string]any) {
				p["source"] = map[string]any{"repository": "http://github.com/acme/api", "revision": "main"}
			},
			wantErr: pipeline.ErrRepositoryScheme,
		},
		{
			name: "a credential in the repository URL",
			mutate: func(p map[string]any) {
				p["source"] = map[string]any{"repository": "https://x:tok@github.com/acme/api", "revision": "main"}
			},
			wantErr: pipeline.ErrRepositoryCredential,
		},
		{
			name:    "a source of the wrong shape",
			mutate:  func(p map[string]any) { p["source"] = "https://github.com/acme/api" },
			wantErr: ErrIncompleteBuildSource,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolver := &stubResolver{commit: testCommit}
			e := New(map[spec.Provider]provider.CloudProvider{
				spec.ProviderAWS: &mockProvider{name: "aws"},
			}, WithRevisionResolver(resolver))

			pipe := pipelineResource("api-build", spec.ProviderAWS, 8080)
			tt.mutate(pipe.Properties)

			_, err := e.resolveBuilds(context.Background(), graphSpec(pipe))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("resolveBuilds() = %v, want %v", err, tt.wantErr)
			}
			if len(resolver.calls) != 0 {
				t.Errorf("contacted the repository despite an invalid source: %v", resolver.calls)
			}
		})
	}
}

// TestValidateResolvesBuilds: the resolution happens inside the Engine's
// own Validate, so a plan shows a commit rather than a branch.
func TestValidateResolvesBuilds(t *testing.T) {
	resolver := &stubResolver{commit: testCommit}
	mock := &mockProvider{name: "aws"}
	e := New(map[spec.Provider]provider.CloudProvider{spec.ProviderAWS: mock},
		WithRevisionResolver(resolver))

	s := graphSpec(
		serviceResource("api", spec.ProviderAWS, 8080, "api-build"),
		pipelineResource("api-build", spec.ProviderAWS, 8080),
	)

	validated, err := e.validated(context.Background(), s)
	if err != nil {
		t.Fatalf("validated() = %v, want nil", err)
	}
	for _, r := range validated.Resources {
		if r.Resolved == nil || r.Resolved.Commit != testCommit {
			t.Errorf("resource %q reached the providers unresolved", r.ID)
		}
	}
}
