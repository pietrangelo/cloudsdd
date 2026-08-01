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

	// EnsureNetwork idempotently provisions the shared network for a
	// scope, before any resource in that scope is applied (RFC 016 §2.2).
	// A provider with nothing to share returns nil.
	//
	// It exists because a network outlives and precedes the resources in
	// it, and so cannot be declared inside any one resource's program:
	// stack identity is per-resource, so a program cannot create
	// something a different stack also needs. Ordering belongs to the
	// Engine, which is the only component that can see every resource in
	// a Specification — a provider deciding for itself would have three
	// concurrent resource programs racing to create one VPC.
	EnsureNetwork(ctx context.Context, s NetworkScope, p spec.Policies) error

	// DestroyNetwork removes the shared network of a scope (RFC 016
	// §2.6). A provider with nothing to share returns nil.
	//
	// The caller is responsible for establishing that the scope is empty
	// first. A network is shared, so destroying one that still holds
	// resources cuts them off from everything, and this method cannot
	// tell — it sees a scope, not the scope's contents.
	DestroyNetwork(ctx context.Context, s NetworkScope, p spec.Policies) error
}

// NetworkScope identifies the boundary one shared network serves
// (RFC 016 §2.1): one per account, per environment, per region.
//
// It is not spec.Scope, which carries neither the account — that lives on
// Resource.Account — nor the provider. Both belong here: an AWS account
// and an Azure subscription are different things that can never share a
// network, so the provider is part of the identity rather than a
// coincidence of who was asked.
type NetworkScope struct {
	// Provider owns the network. Two providers never share one.
	Provider spec.Provider

	// Account is the DeploymentTarget name, empty for the default
	// credentials. Accounts never share a network — an invariant, not a
	// default (RFC 016 §2.1) — so this is the coarsest partition here.
	Account string

	// Environment and Region complete the scope, matching the dimensions
	// RFC 005 §2.6 already uses for stack identity.
	Environment string
	Region      string

	// Sealed is the scope's isolation posture, defaulting to true. A
	// sealed scope may not be peered to another, and no scope may ever be
	// peered across an account boundary regardless of this flag.
	Sealed bool
}
