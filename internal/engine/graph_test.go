// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package engine

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"cloudsdd/internal/spec"
)

// pipelineResource builds a build_pipeline declaring the given ports.
func pipelineResource(id string, p spec.Provider, ports ...int) spec.Resource {
	declared := make([]any, len(ports))
	for i, port := range ports {
		declared[i] = port
	}
	return spec.Resource{
		ID:       id,
		Type:     spec.ResourceTypeBuildPipeline,
		Provider: p,
		Properties: map[string]any{
			"source":     map[string]any{"repository": "https://github.com/acme/api", "revision": "main"},
			"stack":      map[string]any{"runtime": "go", "version": "1.22"},
			"image_name": "acme/api",
			"ports":      declared,
		},
	}
}

// serviceResource builds a container_service consuming a pipeline. An empty
// ref means a service that names an image instead.
func serviceResource(id string, p spec.Provider, port int, ref string) spec.Resource {
	props := map[string]any{"port": port, "size": "small"}
	if ref == "" {
		props["image"] = "acme/api@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	} else {
		props["pipeline"] = ref
	}
	return spec.Resource{ID: id, Type: spec.ResourceTypeContainerService, Provider: p, Properties: props}
}

// databaseResource is the stand-in for "a resource of some other type",
// used where a reference has to land on something that is not a pipeline.
func databaseResource(id string, p spec.Provider) spec.Resource {
	return spec.Resource{
		ID:         id,
		Type:       spec.ResourceTypeRelationalDatabase,
		Provider:   p,
		Properties: map[string]any{"engine": "postgres"},
	}
}

func graphSpec(resources ...spec.Resource) spec.Specification {
	return spec.Specification{SDDVersion: "1.0", Intent: spec.IntentDeploy, Resources: resources}
}

// TestDependencyGraphValidation covers the reference rules RFC 018 §2.9
// introduces: the first cross-resource reference the schema has.
func TestDependencyGraphValidation(t *testing.T) {
	tests := []struct {
		name    string
		s       spec.Specification
		wantErr error
	}{
		{
			name: "a service and the pipeline it consumes",
			s: graphSpec(
				serviceResource("api", spec.ProviderAWS, 8080, "api-build"),
				pipelineResource("api-build", spec.ProviderAWS, 8080),
			),
		},
		{
			// A pipeline can exist before anything consumes it.
			name: "a pipeline nothing consumes",
			s:    graphSpec(pipelineResource("api-build", spec.ProviderAWS, 8080)),
		},
		{
			// Two services fed by one pipeline is the case that makes a
			// pipeline a resource rather than a property (RFC 018 §2.1).
			name: "two services on one pipeline",
			s: graphSpec(
				pipelineResource("api-build", spec.ProviderGCP, 8080, 9090),
				serviceResource("api", spec.ProviderGCP, 8080, "api-build"),
				serviceResource("metrics", spec.ProviderGCP, 9090, "api-build"),
			),
		},
		{
			name: "a specification with no references at all",
			s: graphSpec(
				serviceResource("api", spec.ProviderAWS, 8080, ""),
				pipelineResource("unrelated", spec.ProviderAWS, 8080),
			),
		},
		{
			name: "a reference to a resource that is not there",
			s: graphSpec(
				serviceResource("api", spec.ProviderAWS, 8080, "does-not-exist"),
			),
			wantErr: ErrReferenceNotFound,
		},
		{
			name: "a reference to a resource of the wrong type",
			s: graphSpec(
				serviceResource("api", spec.ProviderAWS, 8080, "app-db"),
				databaseResource("app-db", spec.ProviderAWS),
			),
			wantErr: ErrReferenceWrongType,
		},
		{
			// A service naming itself is caught as a wrong type before it
			// is caught as a cycle, because that is the more actionable of
			// the two reports.
			name: "a service referencing itself",
			s: graphSpec(
				serviceResource("api", spec.ProviderAWS, 8080, "api"),
			),
			wantErr: ErrReferenceWrongType,
		},
		{
			// A reference names one resource. Two resources with one id is
			// a Specification where it names two.
			name: "duplicate resource ids",
			s: graphSpec(
				pipelineResource("api-build", spec.ProviderAWS, 8080),
				pipelineResource("api-build", spec.ProviderAWS, 9090),
			),
			wantErr: ErrDuplicateResourceID,
		},
	}

	g := NewDependencyGraph()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := g.Validate(context.Background(), tt.s)

			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestDependencyGraphValidation_PortAgreement covers RFC 018 §2.3: the
// service routes to one port, the image declares a set, and the first must
// be in the second — checked before anything is built, because a service on
// a port the image never opens starts and answers nothing.
func TestDependencyGraphValidation_PortAgreement(t *testing.T) {
	tests := []struct {
		name          string
		servicePort   int
		declaredPorts []int
		wantErr       error
	}{
		{name: "the only declared port", servicePort: 8080, declaredPorts: []int{8080}},
		{name: "one of several", servicePort: 9090, declaredPorts: []int{8080, 9090}},
		{
			name:          "a port the image does not declare",
			servicePort:   3000,
			declaredPorts: []int{8080},
			wantErr:       ErrPortNotDeclared,
		},
		{
			// `ports` is optional in the schema, but a pipeline something
			// consumes has to say what its image opens, or the agreement is
			// unverifiable and the service is deployed on a hope.
			name:          "a pipeline that declares no ports",
			servicePort:   8080,
			declaredPorts: nil,
			wantErr:       ErrPortNotDeclared,
		},
	}

	g := NewDependencyGraph()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := graphSpec(
				serviceResource("api", spec.ProviderAWS, tt.servicePort, "api-build"),
				pipelineResource("api-build", spec.ProviderAWS, tt.declaredPorts...),
			)

			err := g.Validate(context.Background(), s)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestDependencyGraphValidation_PortAgreementAcrossEncodings: a port
// arrives as a float64 from a parsed JSON document and as an int from a
// Specification built in Go. A check that handled only one would pass on
// exactly the documents it is meant to read.
func TestDependencyGraphValidation_PortAgreementAcrossEncodings(t *testing.T) {
	encodings := map[string]struct {
		port  any
		ports []any
	}{
		"int (built in Go)":         {port: 8080, ports: []any{8080}},
		"float64 (parsed JSON)":     {port: float64(8080), ports: []any{float64(8080)}},
		"mixed":                     {port: 8080, ports: []any{float64(8080)}},
		"float64 service, int pipe": {port: float64(8080), ports: []any{8080}},
	}

	g := NewDependencyGraph()

	for name, enc := range encodings {
		t.Run(name, func(t *testing.T) {
			service := serviceResource("api", spec.ProviderAWS, 0, "api-build")
			service.Properties["port"] = enc.port
			pipe := pipelineResource("api-build", spec.ProviderAWS)
			pipe.Properties["ports"] = enc.ports

			if err := g.Validate(context.Background(), graphSpec(service, pipe)); err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

// TestDependencyGraphValidation_SameProvider covers RFC 018 §7.3. Splitting
// a service from its pipeline across two clouds would work — a registry is
// reachable across clouds — while paying egress on every pull and being
// almost certainly not what anyone meant.
func TestDependencyGraphValidation_SameProvider(t *testing.T) {
	tests := []struct {
		name     string
		service  spec.Provider
		pipeline spec.Provider
		wantErr  error
	}{
		{name: "both on aws", service: spec.ProviderAWS, pipeline: spec.ProviderAWS},
		{name: "both on gcp", service: spec.ProviderGCP, pipeline: spec.ProviderGCP},
		{
			// Both unresolved is not a split: resolution runs before this,
			// and a graph driven directly must not invent a disagreement
			// between two resources that have not chosen yet.
			name:     "both still agnostic",
			service:  spec.ProviderAgnostic,
			pipeline: spec.ProviderAgnostic,
		},
		{
			name:     "a service on aws and a pipeline on gcp",
			service:  spec.ProviderAWS,
			pipeline: spec.ProviderGCP,
			wantErr:  ErrCrossProviderReference,
		},
		{
			// Half-resolved is a split too: it is how the agnostic
			// tie-break would produce one silently.
			name:     "a resolved service and an agnostic pipeline",
			service:  spec.ProviderAzure,
			pipeline: spec.ProviderAgnostic,
			wantErr:  ErrCrossProviderReference,
		},
	}

	g := NewDependencyGraph()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := graphSpec(
				serviceResource("api", tt.service, 8080, "api-build"),
				pipelineResource("api-build", tt.pipeline, 8080),
			)

			err := g.Validate(context.Background(), s)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestDependencyGraphValidation_Cycles covers the ordering algorithm's
// refusal to place a loop.
//
// With one typed reference kind a cycle is not reachable through the public
// schema — a container_service can only point at a build_pipeline, and a
// build_pipeline points at nothing — so these drive the sort directly. RFC
// 018 §2.9 states the rule now precisely because it stops being trivial the
// moment there is a second reference kind, and a rule with no test is a
// rule that was not implemented.
func TestDependencyGraphValidation_Cycles(t *testing.T) {
	// A resource whose `pipeline` names another container_service builds
	// the edge the schema will not: the type check is bypassed, the sort
	// is not.
	linked := func(id, ref string) spec.Resource {
		r := serviceResource(id, spec.ProviderAWS, 8080, ref)
		return r
	}

	tests := []struct {
		name      string
		resources []spec.Resource
		wantStuck []string
	}{
		{
			name:      "a resource depending on itself",
			resources: []spec.Resource{linked("api", "api")},
			wantStuck: []string{"api"},
		},
		{
			name: "two resources depending on each other",
			resources: []spec.Resource{
				linked("api", "worker"),
				linked("worker", "api"),
			},
			wantStuck: []string{"api", "worker"},
		},
		{
			name: "a longer loop",
			resources: []spec.Resource{
				linked("a", "c"),
				linked("b", "a"),
				linked("c", "b"),
			},
			wantStuck: []string{"a", "b", "c"},
		},
		{
			// Everything outside the loop still sorts; only the loop is
			// reported, so the error points at what to fix.
			name: "a loop beside an acyclic pair",
			resources: []spec.Resource{
				pipelineResource("api-build", spec.ProviderAWS, 8080),
				serviceResource("api", spec.ProviderAWS, 8080, "api-build"),
				linked("x", "y"),
				linked("y", "x"),
			},
			wantStuck: []string{"x", "y"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := graphSpec(tt.resources...)

			_, err := topoSort(s)
			if !errors.Is(err, ErrDependencyCycle) {
				t.Fatalf("topoSort() = %v, want %v", err, ErrDependencyCycle)
			}
			for _, id := range tt.wantStuck {
				if !strings.Contains(err.Error(), id) {
					t.Errorf("error %q does not name %q, which is in the cycle", err, id)
				}
			}

			// Validate must refuse the same Specification, whichever of its
			// checks gets there first.
			if err := NewDependencyGraph().Validate(context.Background(), s); err == nil {
				t.Error("Validate() = nil, want an error for a Specification containing a cycle")
			}
		})
	}
}

// TestOrdered covers apply order: a pipeline is applied, and its build
// completed, before any service that consumes it (RFC 018 §2.9).
func TestOrdered(t *testing.T) {
	tests := []struct {
		name string
		s    spec.Specification
		want []string
	}{
		{
			// The case that matters: the service is declared first and
			// applied second.
			name: "a dependency declared after its dependent",
			s: graphSpec(
				serviceResource("api", spec.ProviderAWS, 8080, "api-build"),
				pipelineResource("api-build", spec.ProviderAWS, 8080),
			),
			want: []string{"api-build", "api"},
		},
		{
			name: "a dependency declared before its dependent",
			s: graphSpec(
				pipelineResource("api-build", spec.ProviderAWS, 8080),
				serviceResource("api", spec.ProviderAWS, 8080, "api-build"),
			),
			want: []string{"api-build", "api"},
		},
		{
			// No references: the order is the order it was written in.
			// Anything else would silently reorder every Specification that
			// existed before this code did.
			name: "no references preserves declaration order",
			s: graphSpec(
				serviceResource("c", spec.ProviderAWS, 8080, ""),
				serviceResource("a", spec.ProviderAWS, 8080, ""),
				serviceResource("b", spec.ProviderAWS, 8080, ""),
			),
			want: []string{"c", "a", "b"},
		},
		{
			name: "two dependents of one dependency keep their order",
			s: graphSpec(
				serviceResource("api", spec.ProviderAWS, 8080, "api-build"),
				serviceResource("metrics", spec.ProviderAWS, 9090, "api-build"),
				pipelineResource("api-build", spec.ProviderAWS, 8080, 9090),
			),
			want: []string{"api-build", "api", "metrics"},
		},
	}

	g := NewDependencyGraph()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ordered, err := g.Ordered(tt.s)
			if err != nil {
				t.Fatalf("Ordered() = %v, want nil", err)
			}
			assertIDs(t, "Ordered", ordered, tt.want)

			// ReverseOrdered is the same answer read backwards: a
			// dependent is destroyed before what it depends on.
			reversed, err := g.ReverseOrdered(tt.s)
			if err != nil {
				t.Fatalf("ReverseOrdered() = %v, want nil", err)
			}
			want := make([]string, len(tt.want))
			for i, id := range tt.want {
				want[len(tt.want)-1-i] = id
			}
			assertIDs(t, "ReverseOrdered", reversed, want)
		})
	}
}

// TestOrderedIsDeterministic: the result must not depend on map iteration
// order, or a plan would differ from the apply that followed it.
func TestOrderedIsDeterministic(t *testing.T) {
	s := graphSpec(
		serviceResource("api", spec.ProviderAWS, 8080, "api-build"),
		serviceResource("metrics", spec.ProviderAWS, 9090, "api-build"),
		serviceResource("worker", spec.ProviderAWS, 8080, "worker-build"),
		pipelineResource("api-build", spec.ProviderAWS, 8080, 9090),
		pipelineResource("worker-build", spec.ProviderAWS, 8080),
	)

	g := NewDependencyGraph()
	first, err := g.Ordered(s)
	if err != nil {
		t.Fatalf("Ordered() = %v, want nil", err)
	}
	for i := 0; i < 50; i++ {
		again, err := g.Ordered(s)
		if err != nil {
			t.Fatalf("Ordered() = %v, want nil", err)
		}
		for j := range first {
			if first[j].ID != again[j].ID {
				t.Fatalf("Ordered() is not deterministic: %v then %v", ids(first), ids(again))
			}
		}
	}
}

// TestOrderedKeepsEveryResource: an ordering that drops a resource is an
// apply that silently skips it.
func TestOrderedKeepsEveryResource(t *testing.T) {
	s := graphSpec(
		pipelineResource("api-build", spec.ProviderAWS, 8080),
		serviceResource("api", spec.ProviderAWS, 8080, "api-build"),
		serviceResource("standalone", spec.ProviderAWS, 8080, ""),
		databaseResource("app-db", spec.ProviderAWS),
	)

	for _, tt := range []struct {
		name string
		fn   func(spec.Specification) ([]spec.Resource, error)
	}{
		{name: "Ordered", fn: NewDependencyGraph().Ordered},
		{name: "ReverseOrdered", fn: NewDependencyGraph().ReverseOrdered},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.fn(s)
			if err != nil {
				t.Fatalf("%s() = %v, want nil", tt.name, err)
			}
			if len(got) != len(s.Resources) {
				t.Fatalf("%s() returned %d resources, want %d: %v", tt.name, len(got), len(s.Resources), ids(got))
			}
			seen := map[string]bool{}
			for _, r := range got {
				if seen[r.ID] {
					t.Errorf("%s() returned %q twice", tt.name, r.ID)
				}
				seen[r.ID] = true
			}
			for _, r := range s.Resources {
				if !seen[r.ID] {
					t.Errorf("%s() dropped %q", tt.name, r.ID)
				}
			}
		})
	}
}

func ids(resources []spec.Resource) []string {
	out := make([]string, len(resources))
	for i, r := range resources {
		out[i] = r.ID
	}
	return out
}

func assertIDs(t *testing.T, what string, got []spec.Resource, want []string) {
	t.Helper()
	gotIDs := ids(got)
	if len(gotIDs) != len(want) {
		t.Fatalf("%s() = %v, want %v", what, gotIDs, want)
	}
	for i := range want {
		if gotIDs[i] != want[i] {
			t.Fatalf("%s() = %v, want %v", what, gotIDs, want)
		}
	}
}

// TestGraphIgnoresAPipelinePropertyOfTheWrongShape: a `pipeline` that is
// not a string is not a reference this can read, and inventing one from it
// would be worse than leaving it to the provider's decoder — which rejects
// it by type, in its own vocabulary.
func TestGraphIgnoresAPipelinePropertyOfTheWrongShape(t *testing.T) {
	shapes := map[string]any{
		"a number":  42,
		"a list":    []any{"api-build"},
		"an object": map[string]any{"id": "api-build"},
		"null":      nil,
	}

	g := NewDependencyGraph()

	for name, value := range shapes {
		t.Run(name, func(t *testing.T) {
			service := serviceResource("api", spec.ProviderAWS, 8080, "")
			delete(service.Properties, "image")
			service.Properties["pipeline"] = value

			if err := g.Validate(context.Background(), graphSpec(service)); err != nil {
				t.Fatalf("Validate() = %v, want nil (the decoder's error to report)", err)
			}
		})
	}
}

// TestPortAgreementOnUnreadableProperties: a port of the wrong type is
// reported rather than skipped. A check that returns nil when it cannot
// read its input passes hardest exactly when something is wrong.
func TestPortAgreementOnUnreadableProperties(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(service, pipe *spec.Resource)
		wantIn string
	}{
		{
			name:   "a port that is not a number",
			mutate: func(service, _ *spec.Resource) { service.Properties["port"] = "eight thousand" },
			wantIn: `cannot read ` + "`port`",
		},
		{
			name:   "declared ports that are not numbers",
			mutate: func(_, pipe *spec.Resource) { pipe.Properties["ports"] = []any{"8080"} },
			wantIn: `cannot read ` + "`ports`",
		},
		{
			// A value encoding/json cannot marshal at all.
			name:   "a port that is not representable",
			mutate: func(service, _ *spec.Resource) { service.Properties["port"] = math.NaN() },
			wantIn: `cannot read ` + "`port`",
		},
	}

	g := NewDependencyGraph()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := serviceResource("api", spec.ProviderAWS, 8080, "api-build")
			pipe := pipelineResource("api-build", spec.ProviderAWS, 8080)
			tt.mutate(&service, &pipe)

			err := g.Validate(context.Background(), graphSpec(service, pipe))
			if err == nil {
				t.Fatal("Validate() = nil, want an error for properties it cannot read")
			}
			if !strings.Contains(err.Error(), tt.wantIn) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantIn)
			}
		})
	}
}

// TestOrderingOnBrokenSpecifications covers what the ordering methods do
// with input Validate would have refused. They are separate entry points on
// the interface, so a caller can reach them directly.
func TestOrderingOnBrokenSpecifications(t *testing.T) {
	g := NewDependencyGraph()

	t.Run("a dangling reference still orders everything", func(t *testing.T) {
		// Ordering does not wait for a resource that is not there;
		// reporting the dangling reference is Validate's job, and doing it
		// twice in two vocabularies helps nobody.
		s := graphSpec(
			serviceResource("api", spec.ProviderAWS, 8080, "missing"),
			pipelineResource("api-build", spec.ProviderAWS, 8080),
		)

		ordered, err := g.Ordered(s)
		if err != nil {
			t.Fatalf("Ordered() = %v, want nil", err)
		}
		assertIDs(t, "Ordered", ordered, []string{"api", "api-build"})
	})

	t.Run("a duplicate id is refused by both", func(t *testing.T) {
		s := graphSpec(
			pipelineResource("api-build", spec.ProviderAWS, 8080),
			pipelineResource("api-build", spec.ProviderAWS, 9090),
		)
		if _, err := g.Ordered(s); !errors.Is(err, ErrDuplicateResourceID) {
			t.Errorf("Ordered() = %v, want %v", err, ErrDuplicateResourceID)
		}
		if _, err := g.ReverseOrdered(s); !errors.Is(err, ErrDuplicateResourceID) {
			t.Errorf("ReverseOrdered() = %v, want %v", err, ErrDuplicateResourceID)
		}
	})

	t.Run("a cycle is refused by both", func(t *testing.T) {
		s := graphSpec(
			serviceResource("api", spec.ProviderAWS, 8080, "worker"),
			serviceResource("worker", spec.ProviderAWS, 8080, "api"),
		)
		if _, err := g.Ordered(s); !errors.Is(err, ErrDependencyCycle) {
			t.Errorf("Ordered() = %v, want %v", err, ErrDependencyCycle)
		}
		if _, err := g.ReverseOrdered(s); !errors.Is(err, ErrDependencyCycle) {
			t.Errorf("ReverseOrdered() = %v, want %v", err, ErrDependencyCycle)
		}
	})
}
