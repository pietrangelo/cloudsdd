// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package gcp

import "errors"

// ErrResourceNotSchedulable indicates that a resource explicitly declared
// a power schedule but its ResourceType has no power state (RFC 012 §3).
var ErrResourceNotSchedulable = errors.New("gcp: resource type does not support a power schedule")

// ErrScheduleExceptionsUnsupported indicates a schedule carrying exception
// windows, which GCP cannot express (RFC 012 §4.2, RFC 013 §2.5).
//
// Refusing is the only honest option: a dropped window would leave an
// environment running through a shutdown the user believed they had
// scheduled, and the failure would surface as an invoice rather than an
// error. The two schedulable resource types hit this limit for different
// structural reasons, so the message names the one that applies.
var ErrScheduleExceptionsUnsupported = errors.New("gcp: exception windows are not supported")

// ErrUnsupportedSize indicates a compute_instance size with no Compute
// Engine machine type mapping (RFC 013 §2.1). An unmapped value is a hard
// error rather than a fallback: silently substituting a different machine
// is the failure mode an intent-driven system cannot tolerate.
var ErrUnsupportedSize = errors.New("gcp: unsupported compute size")

// ErrUnsupportedOS indicates a compute_instance image with no Compute
// Engine image family mapping (RFC 013 §2.1).
var ErrUnsupportedOS = errors.New("gcp: unsupported operating system")

// ErrCloudRunNotSchedulable indicates an explicit power schedule on a
// container_service deployed to Cloud Run (RFC 017 §2.5).
//
// Not an implementation gap. Cloud Run bills per request and idles to zero
// between them, so there is no running state to switch off and no saving
// left for a schedule to deliver — the saving is already unconditional.
// RFC 017 §2.5 proposed honouring a schedule by setting max instances to
// zero; step 2 found that Cloud Run reads a zero ceiling as *unset* and
// applies its own default, which would uncap the service rather than stop
// it.
//
// Refused rather than accepted-and-ignored, per RFC 012 §1.3: a schedule
// silently doing nothing is the failure mode that surfaces as an invoice.
// A schedule *inherited* from policies is not an error — it is reported as
// inapplicable in the plan and the service is left as it is.
var ErrCloudRunNotSchedulable = errors.New(
	"gcp: Cloud Run has no power state to schedule; it bills per request and idles to zero between them")

// ErrZonesNotSupported indicates that Scope.Zones was set on a
// ResourceType with no zone-aware placement (RFC 005 §2.5).
//
// Refused rather than ignored: a zone forwarded and dropped is a placement
// the user asked for and did not get, and would surface as a surprise
// invoice for cross-zone traffic rather than as an error.
var ErrZonesNotSupported = errors.New("gcp: resource type does not support scope.zones")

// ErrZeroReplicasUnsupported indicates a container_service asking for zero
// replicas on Cloud Run (RFC 017 §2.5).
//
// The shared schema allows it — "deployed, running nothing" is a state
// RFC 017 §2.2 deliberately makes expressible — and Cloud Run cannot
// express it through scaling. Its maxInstanceCount is an int the API reads
// as unset when it is zero, so a service asking for no instances would be
// created with Google's *default* ceiling instead: the opposite of what
// was written, and the one failure mode an intent-driven system cannot
// tolerate.
//
// Refusing rather than substituting follows RFC 012 §1.3: a rule a
// provider cannot express is a Validate error, never a dropped rule.
// Cloud Run already scales to zero on its own between requests, so what
// this refuses is a way of saying "and never scale up", not the saving
// itself.
var ErrZeroReplicasUnsupported = errors.New(
	"gcp: Cloud Run cannot be pinned to zero replicas; it already scales to zero between requests")
