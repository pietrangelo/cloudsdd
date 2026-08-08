// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"cloudsdd/internal/provider/pipeline"
	"cloudsdd/internal/spec"
)

// resolveBuilds fills Resource.Resolved for every build_pipeline in s and
// for every container_service that references one (RFC 018 §2.4.1).
//
// It runs at the Engine, once per Specification, rather than inside each
// provider: the answer is the same on every cloud, it costs a network
// round-trip per pipeline, and three providers asking the same repository
// the same question would be three chances to get a different answer for
// one Specification.
//
// It returns a copy. Resolve already establishes the pattern that the
// Specification the checks ran against is the one that must be applied.
func (e *DefaultEngine) resolveBuilds(ctx context.Context, s spec.Specification) (spec.Specification, error) {
	// Nothing in this Specification builds anything: no lookups, no
	// copies, and in particular no network access for the overwhelming
	// majority of Specifications that contain no pipeline at all.
	if !hasBuildPipeline(s) {
		return s, nil
	}

	resolved := make(map[string]*spec.Resolved, len(s.Resources))
	resources := append([]spec.Resource(nil), s.Resources...)

	for i, r := range resources {
		if r.Type != spec.ResourceTypeBuildPipeline {
			continue
		}
		source, err := readBuildSource(r)
		if err != nil {
			return spec.Specification{}, err
		}
		commit, err := e.revisions.Resolve(ctx, source.Repository, source.Revision)
		if err != nil {
			return spec.Specification{}, fmt.Errorf("engine: resource %q: %w", r.ID, err)
		}

		answer := &spec.Resolved{ImageName: source.ImageName, Commit: commit}
		resolved[r.ID] = answer
		resources[i].Resolved = answer
	}

	// A service is given its pipeline's answer, not its own: what it runs
	// is what that pipeline publishes. The graph has already established
	// that the reference resolves to a build_pipeline, so a lookup that
	// misses here would mean the two disagree.
	for i, r := range resources {
		ref, ok := pipelineRef(r)
		if !ok {
			continue
		}
		answer, found := resolved[ref]
		if !found {
			return spec.Specification{}, fmt.Errorf("%w: resource %q references pipeline %q", ErrReferenceNotFound, r.ID, ref)
		}
		resources[i].Resolved = answer
	}

	s.Resources = resources
	return s, nil
}

func hasBuildPipeline(s spec.Specification) bool {
	for _, r := range s.Resources {
		if r.Type == spec.ResourceTypeBuildPipeline {
			return true
		}
	}
	return false
}

// buildSource is the subset of a build_pipeline's properties the Engine
// needs to resolve a revision.
type buildSource struct {
	Repository string
	Revision   string
	ImageName  string
}

// readBuildSource reads a pipeline's source out of its generic properties.
//
// The Engine reads raw properties here for the reason it already does for
// `image` and `pipeline` (RFC 011 §2.3): this runs for every provider, and
// must not depend on any one provider's decoder having gone first. It is
// stricter than those two, because a missing value here would mean
// resolving nothing and building whatever the branch happens to point at
// later — silently reintroducing exactly what §2.4 refuses.
func readBuildSource(r spec.Resource) (buildSource, error) {
	var view struct {
		Source struct {
			Repository string `json:"repository"`
			Revision   string `json:"revision"`
		} `json:"source"`
		ImageName string `json:"image_name"`
	}

	raw, err := json.Marshal(r.Properties)
	if err != nil {
		return buildSource{}, fmt.Errorf("engine: resource %q: cannot read `source`: %w", r.ID, err)
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		// A source of the wrong shape is wrapped as an incomplete one:
		// both mean the Engine cannot tell what would be built, and one
		// sentinel for that is easier to act on than two. The provider's
		// decoder reports the shape itself, in its own vocabulary.
		return buildSource{}, fmt.Errorf("engine: resource %q: %w: %v", r.ID, ErrIncompleteBuildSource, err)
	}

	source := buildSource{
		Repository: view.Source.Repository,
		Revision:   view.Source.Revision,
		ImageName:  view.ImageName,
	}
	if source.Repository == "" || source.Revision == "" || source.ImageName == "" {
		return buildSource{}, fmt.Errorf("engine: resource %q: %w", r.ID, ErrIncompleteBuildSource)
	}
	// The shared rules, applied here as well as in each provider's
	// decoder, so an https-only repository and a well-formed revision hold
	// before anything is sent over the network.
	if err := (pipeline.Source{Repository: source.Repository, Revision: source.Revision}).Validate(); err != nil {
		return buildSource{}, fmt.Errorf("engine: resource %q: %w", r.ID, err)
	}
	return source, nil
}
