// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

// Package spec defines the typed representation of the SDD Specification,
// the "Single Source of Truth" described in docs/rfc/001-core-architecture-and-json-schema.md.
package spec

import "cloudsdd/internal/schedule"

// Intent describes the operation requested on the Specification.
type Intent string

const (
	IntentDeploy  Intent = "deploy"
	IntentUpdate  Intent = "update"
	IntentDestroy Intent = "destroy"
	IntentPlan    Intent = "plan"
)

// ResourceType enumerates the resource types supported by the v1.0 schema.
type ResourceType string

const (
	ResourceTypeRelationalDatabase ResourceType = "relational_database"
	ResourceTypeObjectStorage      ResourceType = "object_storage"
	ResourceTypeComputeInstance    ResourceType = "compute_instance"
	ResourceTypeContainerService   ResourceType = "container_service"
	// ResourceTypeCrossAccountRole represents a cross-account IAM role
	// (RFC 003): grants an external AWS account the ability to assume a
	// role with restricted permissions toward specific resources.
	ResourceTypeCrossAccountRole ResourceType = "cross_account_role"
)

// SupportsSchedule reports whether the ResourceType has a power state that
// can be scheduled (RFC 012 §3).
//
// This is a property of the type, not of the cloud: an object store cannot
// be switched off on any provider, and an IAM role costs nothing to leave
// in place. compute_instance and container_service are declared by the
// schema but implemented by no provider yet; they are listed here so that
// the day a provider implements one, scheduling is not silently rejected.
func (t ResourceType) SupportsSchedule() bool {
	switch t {
	case ResourceTypeRelationalDatabase, ResourceTypeComputeInstance, ResourceTypeContainerService:
		return true
	default:
		return false
	}
}

// Provider identifies the target cloud provider of a resource, or
// "agnostic" to delegate its resolution to the Engine.
type Provider string

const (
	ProviderAgnostic Provider = "agnostic"
	ProviderAWS      Provider = "aws"
	ProviderGCP      Provider = "gcp"
	ProviderAzure    Provider = "azure"
)

// Specification is the root representation of the incoming SDD Specification.
type Specification struct {
	SDDVersion string     `json:"sdd_version" validate:"required,eq=1.0"`
	Intent     Intent     `json:"intent" validate:"required,oneof=deploy update destroy plan"`
	Resources  []Resource `json:"resources" validate:"required,min=1,dive"`
	Policies   Policies   `json:"policies"`
}

// Resource represents a single resource requested in the Specification.
//
// Properties intentionally remains a generic JSON transport container:
// each concrete provider is responsible for decoding it into a typed and
// validated struct specific to its own ResourceType, so as to block Mass
// Assignment before the data reaches business logic (see RFC 001, section
// "Security Considerations").
type Resource struct {
	ID       string       `json:"id" validate:"required,resourceid"`
	Type     ResourceType `json:"type" validate:"required,oneof=relational_database object_storage compute_instance container_service cross_account_role"`
	Provider Provider     `json:"provider" validate:"required,oneof=agnostic aws gcp azure"`
	// Account references, by name, a DeploymentTarget configured at the
	// engine level (RFC 004 §2.2), used to apply the resource with
	// credentials assumed toward an external account/project/subscription
	// instead of the default credential chain. Optional: if absent,
	// behavior is unchanged from RFC 002 §2.4. Deliberately never
	// references a secret: only a symbolic name resolved by the engine,
	// never an ARN or a credential.
	Account string `json:"account,omitempty" validate:"omitempty,max=64"`
	// Scope carries the "where" of the resource: logical Environment,
	// Region/Regions, Zones, and the Sealed cross-boundary toggle (RFC 005
	// §2.2). Orthogonal to Account (RFC 004), which carries "which
	// credentials".
	Scope Scope `json:"scope,omitempty"`

	// Schedule overrides the Specification-wide Policies.Schedule for this
	// resource (RFC 012 §2.1), carrying the "when" alongside Scope's
	// "where". Absent means inherit; present replaces the inherited
	// schedule wholesale, including {"enabled": false} to opt a production
	// resource out of a policy that would otherwise power it down.
	Schedule *schedule.Schedule `json:"schedule,omitempty"`

	Properties map[string]any `json:"properties" validate:"required"`
}

// Scope describes where a resource is deployed within its Account (RFC
// 005 §2.2): logical Environment, Region(s), Zones, and whether it is
// allowed to cross an Account/Environment boundary.
type Scope struct {
	// Environment is a free-form logical stage name (e.g. "dev",
	// "staging", "production"), open-ended by design since organizations
	// name their stages differently. Optional: absent means "unscoped",
	// the same behavior as before this field existed.
	Environment string `json:"environment,omitempty" validate:"omitempty,scopename"`

	// Region is the single-region placement for the resource. Mutually
	// exclusive with Regions. Format is provider-specific (AWS/GCP/Azure
	// region strings differ in shape) and is therefore validated by each
	// provider, not here (RFC 005 §2.2), mirroring how "provider: agnostic"
	// resolution already stays cloud-agnostic at this layer.
	Region string `json:"region,omitempty" validate:"omitempty,excluded_with=Regions"`

	// Regions requests multi-region placement: the Engine fans this out
	// into one Plan/Apply/Validate call per entry, each with Region set to
	// a single value (RFC 005 §2.4.2). Mutually exclusive with Region. A
	// single desired region belongs in Region, not a one-element Regions.
	Regions []string `json:"regions,omitempty" validate:"omitempty,min=2,max=10,excluded_with=Region"`

	// Zones optionally pins/spreads the resource across availability
	// zones within its Region. Forwarded to resource-type-specific HA
	// logic; not fanned out like Regions (RFC 005 §2.4.3). No resource
	// type implemented yet consumes it.
	Zones []string `json:"zones,omitempty" validate:"omitempty,min=1,max=10"`

	// Sealed, when true (the default when absent), forbids the resource
	// from declaring any trust or access relationship toward a different
	// Account/Environment than its own (RFC 005 §2.5, "sealed unless
	// otherwise specified"). A resource type that is inherently
	// cross-boundary (e.g. cross_account_role) must set this explicitly
	// to false; enforcement is provider-specific, per ResourceType.
	Sealed *bool `json:"sealed,omitempty"`
}

// EffectiveSealed returns the value of Sealed, or true if absent
// (fail-closed default, RFC 005 §2.2).
func (s Scope) EffectiveSealed() bool { return boolOrDefault(s.Sealed, true) }

func boolOrDefault(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// Policies expresses the global constraints applied to all resources in the Specification.
//
// max_cost_monthly was removed in RFC 011 §2.9. It was declared and
// validated but enforced nowhere, so it read as a guarantee and provided
// none — worse than not offering it at all. Real cost enforcement needs
// per-provider pricing data and a currency/period model, and is deferred
// to its own RFC.
type Policies struct {
	AllowedRegions []string `json:"allowed_regions,omitempty"`

	// Schedule declares, once for the whole Specification, when the
	// schedulable resources are powered on (RFC 012 §2.1). Resource
	// types with no power state ignore it; a resource that declares its
	// own Schedule overrides it.
	Schedule *schedule.Schedule `json:"schedule,omitempty"`

	// ProviderPreference breaks a tie when a resource declares
	// Provider "agnostic" and more than one registered provider can
	// express it (RFC 014 §2.4). Ordered: the first candidate that
	// appears here wins.
	//
	// "agnostic" is excluded from the allowed values on purpose — a
	// preference list that could contain it would be circular.
	ProviderPreference []Provider `json:"provider_preference,omitempty" validate:"omitempty,max=3,unique,dive,oneof=aws gcp azure"`
}

// EffectiveSchedule returns the Schedule governing r: its own if it
// declares one, otherwise the Specification-wide default. A nil result
// means the resource is never powered down.
//
// A Resource that declares a Schedule replaces the inherited one entirely
// rather than merging it field by field. A partial override would let a
// resource silently inherit a stop time its author never saw, which is
// precisely the class of surprise RFC 012 §1.2 exists to prevent.
func EffectiveSchedule(r Resource, p Policies) *schedule.Schedule {
	if r.Schedule != nil {
		return r.Schedule
	}
	return p.Schedule
}
