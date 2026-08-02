// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package aws

import "errors"

// ErrUnsupportedResourceType indicates that AWSProvider does not yet
// handle the requested ResourceType (RFC 002 §1: only object_storage in
// the first increment, cross_account_role added by RFC 003).
var ErrUnsupportedResourceType = errors.New("aws: unsupported resource type")

// ErrMissingRegionPolicy indicates that a cross_account_role resource was
// requested without Policies.AllowedRegions: RFC 003 §2.3 imposes
// fail-closed behavior, a cross-account role with no explicit region
// constraint is not applied.
var ErrMissingRegionPolicy = errors.New("aws: cross_account_role requires a non-empty Policies.AllowedRegions")

// ErrRegionNotAllowed indicates that the region requested by the resource
// is not included in Policies.AllowedRegions (RFC 001 §3, RFC 002 §2.5).
var ErrRegionNotAllowed = errors.New("aws: region not in allowed_regions")

// ErrMissingPassphrase indicates that the CLOUDSDD_PULUMI_PASSPHRASE
// environment variable is not set (RFC 002 §2.3): the provider refuses to
// start with a weak default.
var ErrMissingPassphrase = errors.New("aws: CLOUDSDD_PULUMI_PASSPHRASE must be set")

// ErrRegionRequired indicates that a region-scoped ResourceType (e.g.
// object_storage) was requested without Scope.Region (RFC 005 §2.2): the
// field moved out of the provider-specific Properties (RFC 002 §2.6) into
// the cloud-agnostic Scope, but object_storage still requires one.
var ErrRegionRequired = errors.New("aws: resource requires a non-empty scope.region")

// ErrZonesNotSupported indicates that Scope.Zones was set on a
// ResourceType that does not yet consume it (RFC 005 §2.4.3): every
// ResourceType implemented today falls in this case.
var ErrZonesNotSupported = errors.New("aws: resource type does not support scope.zones")

// ErrGlobalResourceScoped indicates that Scope.Region/Regions/Zones was
// set on a ResourceType that is global by nature (e.g. cross_account_role,
// IAM is global, RFC 003 §2.3): a global resource does not "live" in a
// region, so this RFC 005 rule replaces the previous implicit behavior.
var ErrGlobalResourceScoped = errors.New("aws: resource type is global and does not support region/zone scoping")

// ErrSealedCrossAccountRole indicates that a cross_account_role resource
// (inherently cross-account by design, RFC 003) was declared without
// explicitly setting scope.sealed to false (RFC 005 §2.5, "sealed unless
// otherwise specified"): the default Sealed == true forbids any resource
// from crossing an Account/Environment boundary implicitly.
var ErrSealedCrossAccountRole = errors.New("aws: cross_account_role requires scope.sealed=false (it is inherently cross-account)")

// ErrResourceNotSchedulable indicates that a resource explicitly declared
// a power schedule but its ResourceType has no power state (RFC 012 §3).
// An S3 bucket cannot be switched off, and an IAM role costs nothing to
// leave in place.
var ErrResourceNotSchedulable = errors.New("aws: resource type does not support a power schedule")

// ErrUnsupportedSize indicates a compute_instance size that this provider
// has no SKU mapping for (RFC 013 §2.1). An unmapped size is a hard error
// rather than a fallback: silently substituting a different machine is the
// failure mode an intent-driven system cannot tolerate.
var ErrUnsupportedSize = errors.New("aws: unsupported compute size")

// ErrUnsupportedOS indicates a compute_instance image that this provider
// has no mapping for (RFC 013 §2.1).
var ErrUnsupportedOS = errors.New("aws: unsupported operating system")

// ErrMalformedARN indicates that an ARN the provider needed to read the
// account or region out of does not have the expected shape (RFC 012
// §4.1).
var ErrMalformedARN = errors.New("aws: malformed ARN")

// ErrUnsupportedContainerSize indicates a container_service size with no
// Fargate CPU/memory pair (RFC 017 §2.2).
//
// Separate from ErrUnsupportedSize, which is compute_instance's: the two
// share a vocabulary and nothing else, and a single error would make a
// message about machine types appear for a container.
var ErrUnsupportedContainerSize = errors.New("aws: unsupported container size")

// ErrPublicRequiresDomain indicates a public container_service with no
// `domain` (RFC 017 §2.3.1).
//
// ACM will not issue a certificate for an ALB's own *.elb.amazonaws.com
// name, and AWS has no equivalent of Cloud Run's *.run.app, so a public
// service with no hostname could only be served over plain HTTP — which
// RFC 017 §2.3 refuses. Refusing the Specification instead follows RFC 012
// §1.3: a request a provider cannot express is an error, never a silent
// downgrade to something weaker than what was asked for.
var ErrPublicRequiresDomain = errors.New(
	"aws: a public container_service requires `domain`; AWS cannot issue a certificate for a load balancer's own name")

// ErrPublicSubnetsMissing indicates a scope network without the public
// tier an internet-facing load balancer needs (RFC 017 §2.3.1).
//
// It means the network predates the tier or was altered out of band. Named
// rather than left to the ALB API, whose own error for a single-subnet
// request does not mention CloudSDD's layout at all.
var ErrPublicSubnetsMissing = errors.New("aws: the scope network has no public subnets")
