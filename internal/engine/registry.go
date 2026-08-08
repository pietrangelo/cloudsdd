// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package engine

import (
	"errors"
	"fmt"

	"cloudsdd/internal/provider/container"
	"cloudsdd/internal/spec"
)

// ErrImagePropertyMissing indicates a container_service that names neither
// an `image` nor a `pipeline`, or whose `image` is not a string.
//
// It is an error rather than a skip. The provider's own decoder would
// reject the same Specification a moment later, but this check is the one
// that runs for every provider (RFC 011 §2.3), and a check that returns nil
// when it cannot read its input is a check that passes hardest exactly when
// something is wrong.
var ErrImagePropertyMissing = errors.New("container_service declares neither an `image` nor a `pipeline` property")

// validateImagePolicy enforces Policies.AllowedRegistries and the image
// pinning rules for a container_service (RFC 017 §2.4).
//
// It lives at the Engine, alongside AllowedRegions and the schedule
// compilation, for the reason RFC 011 §2.3 recorded: a check that lives
// only inside providers is a check the next provider forgets. Providers
// still apply it in their own Validate, so one driven directly — outside
// the Engine — is no less safe.
//
// Resource types other than container_service have no image and are
// skipped. This is the only property in the schema whose value decides what
// code runs, so it is also the only one enforced centrally on content
// rather than on shape.
func validateImagePolicy(r spec.Resource, policies spec.Policies) error {
	if r.Type != spec.ResourceTypeContainerService {
		return nil
	}

	// A service fed by a pipeline names no image, and the one it will run
	// does not exist yet (RFC 018 §2.4). The rules that replace these —
	// exactly one source, and a reference that resolves to a build_pipeline
	// — are the dependency graph's, which runs beside this check.
	if _, viaPipeline := r.Properties["pipeline"]; viaPipeline {
		return nil
	}

	raw, ok := r.Properties["image"]
	if !ok {
		return fmt.Errorf("engine: resource %q: %w", r.ID, ErrImagePropertyMissing)
	}
	image, ok := raw.(string)
	if !ok {
		return fmt.Errorf("engine: resource %q: %w: got %T", r.ID, ErrImagePropertyMissing, raw)
	}

	if err := container.ValidateImage(image, policies.AllowedRegistries); err != nil {
		return fmt.Errorf("engine: resource %q: %w", r.ID, err)
	}
	return nil
}
