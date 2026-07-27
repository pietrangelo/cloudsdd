// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

// Package provider defines the cloud-agnostic contract that every concrete
// backend (AWS, GCP, Azure, ...) must implement, as per
// docs/rfc/001-core-architecture-and-json-schema.md §2.3.
package provider

import (
	"context"

	"cloudsdd/internal/spec"
)

// Action describes the operation a Diff represents for a resource.
type Action string

const (
	ActionCreate  Action = "create"
	ActionUpdate  Action = "update"
	ActionDestroy Action = "destroy"
	ActionNoop    Action = "noop"
)

// Diff represents the gap between the desired state (from the
// Specification) and the current state detected by the provider for a
// single resource, before it is applied.
type Diff struct {
	ResourceID string `json:"resource_id"`
	// Region is the effective region this Diff was computed for, set by
	// the Engine's multi-region fan-out (RFC 005 §2.4.2, §2.7). Empty for
	// an unscoped resource.
	Region  string         `json:"region,omitempty"`
	Action  Action         `json:"action"`
	Changes map[string]any `json:"changes,omitempty"`
}

// Status describes the outcome of applying a resource.
type Status string

const (
	StatusApplied   Status = "applied"
	StatusFailed    Status = "failed"
	StatusDestroyed Status = "destroyed"
)

// Result represents the outcome of applying (or destroying) a single resource.
type Result struct {
	ResourceID string `json:"resource_id"`
	// Region is the effective region this Result was produced for, set by
	// the Engine's multi-region fan-out (RFC 005 §2.4.2, §2.7). Empty for
	// an unscoped resource.
	Region  string         `json:"region,omitempty"`
	Status  Status         `json:"status"`
	Details map[string]any `json:"details,omitempty"`
}

// CloudProvider is the contract implemented by every concrete cloud
// backend. Implementations are responsible for decoding Resource.Properties
// into a struct typed for the ResourceType they handle, validating it
// before every operation (defense against Mass Assignment at the provider
// level).
//
// Every method also receives spec.Policies (RFC 002 §2.5): without it a
// provider would have no way to enforce constraints such as
// Policies.AllowedRegions, which RFC 001 §3 explicitly lists as a
// security/cost defense.
type CloudProvider interface {
	// Name identifies the provider (e.g. "aws", "gcp", "azure").
	Name() string

	// Validate checks that the resource can be expressed by this provider
	// and complies with the Specification's Policies, without making any
	// call to real infrastructure.
	Validate(ctx context.Context, r spec.Resource, p spec.Policies) error

	// Plan computes the Diff between the desired and current state,
	// without applying any change.
	Plan(ctx context.Context, r spec.Resource, p spec.Policies) (Diff, error)

	// Apply idempotently applies the desired state for the resource.
	Apply(ctx context.Context, r spec.Resource, p spec.Policies) (Result, error)

	// Destroy removes the resource from real infrastructure.
	Destroy(ctx context.Context, r spec.Resource, p spec.Policies) error
}
