// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

// Package engine orchestrates applying an SDD Specification onto the
// registered CloudProviders, as per
// docs/rfc/001-core-architecture-and-json-schema.md §2.3.
package engine

import (
	"context"

	"cloudsdd/internal/provider"
	"cloudsdd/internal/spec"
)

// Engine exposes the lifecycle of a Specification: validation, plan
// (diff) computation, and apply, without knowing the details of any
// concrete provider.
type Engine interface {
	// Validate checks the Specification (domain validation) and, for each
	// resource, delegates to the resolved provider's specific validation.
	Validate(ctx context.Context, s spec.Specification) error

	// Plan computes the Diff for every resource in the Specification
	// without applying any change.
	Plan(ctx context.Context, s spec.Specification) ([]provider.Diff, error)

	// Apply applies the Specification, resource by resource, through the
	// registered CloudProviders.
	Apply(ctx context.Context, s spec.Specification) ([]provider.Result, error)
}
