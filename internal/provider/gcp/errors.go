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
