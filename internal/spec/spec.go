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
	// ResourceTypeBuildPipeline builds a container image from source and
	// publishes it to a registry CloudSDD owns (RFC 018 §2.1).
	//
	// It is a ResourceType rather than a property of container_service
	// because it has its own lifecycle: it is created, it runs, it is
	// destroyed. Two services can be fed by one pipeline, and a pipeline can
	// exist before anything consumes it.
	ResourceTypeBuildPipeline ResourceType = "build_pipeline"
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
// in place. Where a *particular cloud* cannot honour a schedule for a type
// that otherwise supports one, that is SupportsScheduleOn's business.
func (t ResourceType) SupportsSchedule() bool {
	switch t {
	case ResourceTypeRelationalDatabase, ResourceTypeComputeInstance, ResourceTypeContainerService:
		return true
	default:
		return false
	}
}

// MountsFilesystem reports whether the ResourceType has somewhere to mount
// a Volume (RFC 020 §2.4, narrowing RFC 018 §2.7).
//
// Like SupportsSchedule, this is a property of the type and not of the
// cloud: a database manages its own storage, an object store is reached by
// SDK and not by path, and an IAM role has no filesystem at all. A
// container_service runs a long-lived image and can be handed a directory
// inside it, so it is the one type that mounts.
//
// A build_pipeline was the second until RFC 020 §2.4 withdrew it, and the
// reason is worth keeping here because it is not an implementation gap a
// later commit should close. Cloud Build and ACR Tasks cannot mount a
// filesystem at all. CodeBuild can, but only for a build placed inside the
// VPC — and RFC 018 runs the build in CodeBuild's own managed network on
// purpose. Honouring the field would mean two providers refusing it and
// the third contradicting RFC 018. A pipeline that wants a cache wants
// each service's native build cache, which is a different feature.
//
// A compute_instance is deliberately not one of them either. It has a
// filesystem, but attaching a shared one to a VM means the guest OS mounts
// it at boot, which is a machine-image concern rather than a resource
// declaration.
func (t ResourceType) MountsFilesystem() bool {
	return t == ResourceTypeContainerService
}

// SupportsScheduleOn narrows SupportsSchedule to one cloud (RFC 017 §2.5).
//
// There is exactly one exception today, and it is not an implementation
// gap. **Cloud Run** bills per request and idles to zero between them, so
// there is no running state for a schedule to switch off — and no saving
// for it to deliver, because the saving is already unconditional. RFC 017
// §2.5 proposed honouring a schedule by setting max instances to zero;
// step 2 found that Cloud Run reads a zero ceiling as *unset* and applies
// its own default, so that would uncap the service rather than stop it.
//
// The fact lives here, next to SupportsSchedule, rather than only inside
// the GCP provider, because two places have to agree on it: the Engine
// refuses an explicit schedule and reports an inherited one as
// inapplicable, and the provider refuses again as defence in depth. Two
// copies of this answer would drift (RFC 011 §1.1H).
//
// An unresolved "agnostic" provider is treated as supporting the schedule:
// resolution runs before anything consults this, and answering for a cloud
// that has not been chosen would report a limitation that may not apply.
func (t ResourceType) SupportsScheduleOn(p Provider) bool {
	if !t.SupportsSchedule() {
		return false
	}
	return !(p == ProviderGCP && t == ResourceTypeContainerService)
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
	Type     ResourceType `json:"type" validate:"required,oneof=relational_database object_storage compute_instance container_service build_pipeline cross_account_role"`
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

	// Volumes declares the filesystems the resource mounts (RFC 018
	// §2.7). It belongs here beside Scope rather than in Properties for
	// the reason Scope does: a filesystem is part of the "where". It is
	// created in the scope's network, it outlives the resource that
	// mounts it, and everything in the scope naming it gets the same one.
	//
	// Which types may declare one is enforced in Validate rather than by
	// a tag, because the rule constrains the pair (Type, Volumes) and
	// neither field can state it alone.
	//
	// Capped for the reason every other list in a Specification is (RFC
	// 005 §3): each entry is a filesystem to provision, and an unbounded
	// list is an unbounded amount of infrastructure.
	Volumes []Volume `json:"volumes,omitempty" validate:"omitempty,max=5,dive"`

	Properties map[string]any `json:"properties" validate:"required"`

	// Resolved carries what the Engine worked out about this resource that
	// is not in the document (RFC 018 §2.4.1).
	//
	// It has **no JSON tag**, deliberately. Everything else here is
	// user-supplied and treated as hostile; this is the opposite, and a
	// field a Specification could set would be a way to name an image
	// without going through the pinning rules — the mass-assignment
	// problem the whole two-tier decoding exists to prevent.
	Resolved *Resolved `json:"-"`
}

// Resolved is the Engine's answer about a resource's build (RFC 018
// §2.4.1).
//
// One shape serves both sides of a `pipeline` reference, because both need
// the same two facts. On a build_pipeline it is what that pipeline builds
// and publishes; on a container_service it is what the pipeline it
// references will have published by the time the service is applied.
//
// The registry host is absent on purpose: only a provider knows it. An ECR
// host carries the account id, an Artifact Registry path the project, an
// ACR login server the registry's own name.
type Resolved struct {
	// ImageName is the repository within the registry CloudSDD creates.
	ImageName string

	// Commit is the commit `source.revision` resolved to. It is the tag
	// the build publishes under, and so the tag the service asks for —
	// never `latest`, and never a branch (RFC 018 §2.4).
	Commit string
}

// CommitOr returns the commit the Engine resolved, or fallback when it
// supplied none (RFC 019 §2.2).
//
// The fallback exists because a provider can be driven directly, outside
// the Engine, and refusing there would make the provider unusable on its
// own. Driven through the Engine — every path a user takes — a commit is
// always present, so callers pass the revision as written as the fallback.
func (r *Resolved) CommitOr(fallback string) string {
	if r == nil || r.Commit == "" {
		return fallback
	}
	return r.Commit
}

// Complete reports whether r carries both halves of the RFC 018 §2.4.1
// hand-off: the repository the pipeline publishes to, and the tag it
// publishes under.
//
// A half-filled reference is incomplete rather than completable. The two
// plausible inventions — the pipeline's resource id in place of the image
// name, or `latest` in place of the commit — are respectively wrong and
// the one tag RFC 017 §2.4 refuses outright.
func (r *Resolved) Complete() bool {
	return r != nil && r.ImageName != "" && r.Commit != ""
}

// Volume is one filesystem mounted into a resource (RFC 018 §2.7): EFS on
// AWS, Filestore on GCP, Azure Files on Azure.
//
// Only filesystems are expressible. Object storage mounted as a filesystem
// is not one — no atomic rename, no locking, listings that cost money and
// lie under concurrency — so RFC 018 §2.7 defers it rather than shipping a
// sidecar on the two clouds where it is not native. A user who wants a
// bucket declares an object_storage and gets an SDK.
type Volume struct {
	// Name identifies the filesystem within its scope: two resources
	// naming the same volume in the same scope mount the same one. It
	// carries the same charset as Scope.Environment for the same reason
	// — it becomes part of a name a cloud has to accept.
	Name string `json:"name" validate:"required,scopename"`

	// MountPath is where the filesystem appears inside the container.
	// Absolute, no "." or ".." segment, and never "/" itself: mounting
	// over the root would hide the image the resource was built from.
	MountPath string `json:"mount_path" validate:"required,mountpath"`

	// SizeGB is the requested capacity, in gibibytes. Absent means "let
	// the provider decide", which is not laziness: EFS capacity is
	// elastic and has no size to request, so a value invented here would
	// be a number one of the three clouds must then discard.
	SizeGB int `json:"size_gb,omitempty" validate:"omitempty,min=1,max=65536"`
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

	// AllowedRegistries names the container registries a container_service
	// may pull from (RFC 017 §2.4). Absent means none is allow-listed,
	// which is not "anything goes": with no allowlist every image must be
	// pinned to a digest, so the permissive-looking default is the strict
	// one. Naming a registry is how an operator takes responsibility for
	// what its mutable tags point at.
	//
	// Capped at ten for the reason every other policy list is (RFC 005
	// §3): an unbounded list in a Specification is an unbounded amount of
	// work for whatever consumes it.
	AllowedRegistries []string `json:"allowed_registries,omitempty" validate:"omitempty,max=10,unique,dive,required,max=253"`

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

	// Network is the operator's address plan for the networks CloudSDD
	// creates (RFC 016 §2.4.2). Absent means "derive everything from the
	// default block", which is the case that needs no configuration.
	Network *NetworkPolicy `json:"network,omitempty"`
}

// NetworkPolicy confines and, where necessary, overrides the address
// ranges CloudSDD derives for its networks (RFC 016 §2.4).
//
// Both fields are optional. They exist for an operator who already has an
// address plan — "this account gets 172.20.0.0/14 and nothing else" — not
// for the ordinary case, where a user who never mentions networking still
// gets private, non-overlapping networks.
type NetworkPolicy struct {
	// BaseCIDR is the block scope networks are carved out of. Must be
	// RFC 1918 and large enough for at least one scope; validated in
	// internal/provider/network, where the rest of the address
	// arithmetic lives.
	BaseCIDR string `json:"base_cidr,omitempty"`

	// Scopes pins individual scopes, keyed "account::environment::region"
	// with empty segments omitted. It is the escape hatch when two scopes
	// derive the same range, and the way to honour a plan that assigns
	// ranges by hand.
	//
	// Capped for the same reason every other policy list is (RFC 005 §3):
	// an unbounded map in a Specification is an unbounded amount of work
	// for whatever consumes it.
	Scopes map[string]string `json:"scopes,omitempty" validate:"omitempty,max=32"`
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
