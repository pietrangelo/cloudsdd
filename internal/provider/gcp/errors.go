// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package gcp

import "errors"

// ErrResourceNotSchedulable indicates that a resource explicitly declared
// a power schedule but its ResourceType has no power state (RFC 012 §3).
var ErrResourceNotSchedulable = errors.New("gcp: resource type does not support a power schedule")

// ErrScheduleExceptionsUnsupported indicates a schedule carrying exception
// windows, which GCP cannot express (RFC 012 §4.2).
//
// A Cloud Scheduler job has no start or expiry date, so a window cannot
// bound the rules it suspends. Refusing is the only honest option: a
// dropped window would leave an environment running through a shutdown
// the user believed they had scheduled, and the failure would surface as
// an invoice rather than an error.
var ErrScheduleExceptionsUnsupported = errors.New(
	"gcp: exception windows are not supported (Cloud Scheduler jobs have no validity period); " +
		"remove schedule.exceptions or deploy this resource on AWS or Azure")
