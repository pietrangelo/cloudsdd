// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

// Package spec defines the typed representation of the SDD Specification,
// the "Single Source of Truth" described in docs/rfc/001-core-architecture-and-json-schema.md.
package spec

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
	Scope      Scope          `json:"scope,omitempty"`
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
type Policies struct {
	MaxCostMonthly *float64 `json:"max_cost_monthly,omitempty" validate:"omitempty,gt=0"`
	AllowedRegions []string `json:"allowed_regions,omitempty"`
}
