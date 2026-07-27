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
