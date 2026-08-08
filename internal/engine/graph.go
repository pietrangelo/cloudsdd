// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"cloudsdd/internal/spec"
)

// DependencyGraph models the cross-resource references (e.g. container_service -> build_pipeline)
// defined in RFC 018 §2.9. It ensures that references are valid and establishes execution order.
type DependencyGraph interface {
	// Validate ensures all referenced resources exist in the Specification,
	// match the expected ResourceType, and that no cyclical dependencies exist.
	Validate(ctx context.Context, s spec.Specification) error

	// Ordered returns the resources in the order they must be applied
	// (dependencies are placed before their dependents).
	Ordered(s spec.Specification) ([]spec.Resource, error)

	// ReverseOrdered returns the resources in the order they must be destroyed
	// (dependents are placed before their dependencies).
	ReverseOrdered(s spec.Specification) ([]spec.Resource, error)
}

// Reference and ordering errors (RFC 018 §2.9).
var (
	// ErrDuplicateResourceID indicates two resources sharing one id.
	//
	// It was harmless while nothing referred to a resource by name, and
	// stopped being harmless the moment something did: a reference to a
	// duplicated id names two resources, and no reading of it is right.
	ErrDuplicateResourceID = errors.New("engine: duplicate resource id")

	// ErrReferenceNotFound indicates a reference to a resource absent from
	// the Specification.
	ErrReferenceNotFound = errors.New("engine: referenced resource not found")

	// ErrReferenceWrongType indicates a reference to a resource of a type
	// that cannot satisfy it — a `pipeline` naming a database, say.
	ErrReferenceWrongType = errors.New("engine: referenced resource is of the wrong type")

	// ErrDependencyCycle indicates resources that depend on each other,
	// directly or through a chain. There is no order in which to apply
	// them, so the Specification is refused rather than applied partially.
	ErrDependencyCycle = errors.New("engine: circular dependency between resources")

	// ErrPortNotDeclared indicates a container_service routing to a port
	// its pipeline's image does not expose (RFC 018 §2.3). Caught before
	// anything is built: a service on a port the image never opens is a
	// deployment that starts and answers nothing.
	ErrPortNotDeclared = errors.New("engine: container_service port is not declared by its pipeline")

	// ErrCrossProviderReference indicates a service and its pipeline
	// resolving to different clouds (RFC 018 §7.3).
	//
	// It would work — a registry is reachable across clouds — while being
	// almost certainly not what anyone meant, and while paying egress on
	// every image pull. Refused for the reason RFC 014 §2.1 refuses an
	// ambiguous resolution: a Specification whose blast radius depends on
	// which cloud won a tie-break is one nobody reviewed.
	ErrCrossProviderReference = errors.New("engine: a resource and the resource it references resolve to different providers")
)

// dependencyGraph is the reference implementation of DependencyGraph.
//
// It holds no state: every method derives the graph from the Specification
// it is given. A graph cached between calls would be a graph that could
// disagree with the Specification being applied, which is the one thing it
// exists to prevent.
type dependencyGraph struct{}

// NewDependencyGraph returns the DependencyGraph the Engine validates and
// orders with.
func NewDependencyGraph() DependencyGraph { return dependencyGraph{} }

var _ DependencyGraph = dependencyGraph{}

// index builds the id -> resource lookup every other step needs, refusing
// duplicates.
func index(s spec.Specification) (map[string]spec.Resource, error) {
	byID := make(map[string]spec.Resource, len(s.Resources))
	for _, r := range s.Resources {
		if _, clash := byID[r.ID]; clash {
			return nil, fmt.Errorf("%w: %q", ErrDuplicateResourceID, r.ID)
		}
		byID[r.ID] = r
	}
	return byID, nil
}

// pipelineRef reads the `pipeline` property of a container_service, and
// reports whether it names one.
//
// The Engine reads raw properties here, as it already does for `image`
// (RFC 011 §2.3): this check runs for every provider, and it must not
// depend on any one provider's decoder having run first. A property of the
// wrong shape is left to that decoder to report — what this must not do is
// silently read a reference that is not there.
func pipelineRef(r spec.Resource) (string, bool) {
	if r.Type != spec.ResourceTypeContainerService {
		return "", false
	}
	raw, ok := r.Properties["pipeline"]
	if !ok {
		return "", false
	}
	ref, ok := raw.(string)
	if !ok {
		return "", false
	}
	return ref, true
}

// dependencies maps each resource id onto the ids it must be applied
// after.
//
// One reference kind exists today. It is written as a general edge set
// anyway, because the ordering and cycle logic below is the part that is
// hard to get right, and writing it for one case would mean writing it
// again for the second (RFC 018 §2.9).
func dependencies(s spec.Specification) map[string][]string {
	deps := make(map[string][]string, len(s.Resources))
	for _, r := range s.Resources {
		if ref, ok := pipelineRef(r); ok {
			deps[r.ID] = append(deps[r.ID], ref)
		}
	}
	return deps
}

// Validate checks every reference in s, and that the result can be ordered.
func (g dependencyGraph) Validate(ctx context.Context, s spec.Specification) error {
	byID, err := index(s)
	if err != nil {
		return err
	}

	for _, r := range s.Resources {
		ref, ok := pipelineRef(r)
		if !ok {
			continue
		}
		target, found := byID[ref]
		if !found {
			return fmt.Errorf("%w: resource %q references pipeline %q", ErrReferenceNotFound, r.ID, ref)
		}
		if target.Type != spec.ResourceTypeBuildPipeline {
			return fmt.Errorf("%w: resource %q references %q, which is a %s, not a %s",
				ErrReferenceWrongType, r.ID, ref, target.Type, spec.ResourceTypeBuildPipeline)
		}
		if r.Provider != target.Provider {
			return fmt.Errorf("%w: resource %q is on %s and pipeline %q is on %s",
				ErrCrossProviderReference, r.ID, r.Provider, ref, target.Provider)
		}
		if err := validatePortAgreement(r, target); err != nil {
			return err
		}
	}

	// Ordering is the last check because it is the only one whose error
	// cannot name a single culprit, and a cycle report is far less useful
	// than "this reference does not resolve" when both are true.
	if _, err := topoSort(s); err != nil {
		return err
	}
	return nil
}

// portView reads the single field this check needs out of a resource's
// generic properties.
//
// Re-marshalling through encoding/json rather than type-asserting the map
// is what makes it agree with the providers' own decoding: a port arrives
// as a float64 from a parsed document and as an int from a Specification
// built in Go, and a check that handled only one would pass on documents it
// never actually read.
type portView struct {
	Port  int   `json:"port"`
	Ports []int `json:"ports"`
}

func readPorts(props map[string]any) (portView, error) {
	raw, err := json.Marshal(props)
	if err != nil {
		return portView{}, err
	}
	var v portView
	if err := json.Unmarshal(raw, &v); err != nil {
		return portView{}, err
	}
	return v, nil
}

// validatePortAgreement enforces RFC 018 §2.3: the port a service routes to
// must be one its pipeline's image declares.
//
// A pipeline that declares no ports agrees with nothing. That is
// deliberate: `ports` is optional in the schema, but a pipeline something
// consumes has to say what its image opens, or the agreement this checks is
// unverifiable and the service is deployed on a hope.
func validatePortAgreement(service, pipe spec.Resource) error {
	svc, err := readPorts(service.Properties)
	if err != nil {
		// Unreadable properties are the provider decoder's error to
		// report, in its own vocabulary. Skipping here would be the
		// "passes hardest when something is wrong" failure, but so would
		// inventing a port, so this reports what it saw.
		return fmt.Errorf("engine: resource %q: cannot read `port`: %w", service.ID, err)
	}
	pipeline, err := readPorts(pipe.Properties)
	if err != nil {
		return fmt.Errorf("engine: resource %q: cannot read `ports`: %w", pipe.ID, err)
	}

	for _, declared := range pipeline.Ports {
		if declared == svc.Port {
			return nil
		}
	}
	return fmt.Errorf("%w: resource %q routes to port %d, pipeline %q declares %v",
		ErrPortNotDeclared, service.ID, svc.Port, pipe.ID, pipeline.Ports)
}

// topoSort returns the resources in apply order: a dependency always
// precedes the resource that references it.
//
// Kahn's algorithm, with ties broken by declaration order so that a
// Specification with no references at all comes back exactly as written —
// which is what keeps this from silently reordering every existing
// Specification the day it was introduced.
func topoSort(s spec.Specification) ([]spec.Resource, error) {
	byID, err := index(s)
	if err != nil {
		return nil, err
	}
	deps := dependencies(s)

	// remaining counts the unsatisfied dependencies of each resource, and
	// dependents inverts the edges so that satisfying one can release them.
	remaining := make(map[string]int, len(s.Resources))
	dependents := make(map[string][]string, len(s.Resources))
	for _, r := range s.Resources {
		for _, dep := range deps[r.ID] {
			// A reference to something absent is Validate's error to
			// report, with a message that names it. Ordering simply does
			// not wait for a resource that is not there.
			if _, ok := byID[dep]; !ok {
				continue
			}
			remaining[r.ID]++
			dependents[dep] = append(dependents[dep], r.ID)
		}
	}

	// position is where each resource was declared. It breaks ties among
	// resources that become ready together, so the result never depends on
	// map iteration order.
	position := make(map[string]int, len(s.Resources))
	for i, r := range s.Resources {
		position[r.ID] = i
	}

	ready := make([]spec.Resource, 0, len(s.Resources))
	for _, r := range s.Resources {
		if remaining[r.ID] == 0 {
			ready = append(ready, r)
		}
	}

	ordered := make([]spec.Resource, 0, len(s.Resources))
	for len(ready) > 0 {
		r := ready[0]
		ready = ready[1:]
		ordered = append(ordered, r)

		released := make([]spec.Resource, 0, len(dependents[r.ID]))
		for _, id := range dependents[r.ID] {
			remaining[id]--
			if remaining[id] == 0 {
				released = append(released, byID[id])
			}
		}
		sort.SliceStable(released, func(i, j int) bool {
			return position[released[i].ID] < position[released[j].ID]
		})
		ready = append(ready, released...)
	}

	if len(ordered) != len(s.Resources) {
		return nil, fmt.Errorf("%w: %s", ErrDependencyCycle, strings.Join(unordered(s, ordered), ", "))
	}
	return ordered, nil
}

// unordered names the resources a cycle left unplaced, so the error points
// at the loop rather than at the Specification as a whole.
func unordered(s spec.Specification, ordered []spec.Resource) []string {
	placed := make(map[string]bool, len(ordered))
	for _, r := range ordered {
		placed[r.ID] = true
	}
	var stuck []string
	for _, r := range s.Resources {
		if !placed[r.ID] {
			stuck = append(stuck, r.ID)
		}
	}
	return stuck
}

// Ordered returns the resources in apply order.
func (g dependencyGraph) Ordered(s spec.Specification) ([]spec.Resource, error) {
	return topoSort(s)
}

// ReverseOrdered returns the resources in destroy order.
//
// It is Ordered reversed rather than a second traversal: the destroy order
// is the apply order read backwards by definition, and computing it
// separately would be two chances to disagree about one answer.
func (g dependencyGraph) ReverseOrdered(s spec.Specification) ([]spec.Resource, error) {
	ordered, err := topoSort(s)
	if err != nil {
		return nil, err
	}
	reversed := make([]spec.Resource, len(ordered))
	for i, r := range ordered {
		reversed[len(ordered)-1-i] = r
	}
	return reversed, nil
}
