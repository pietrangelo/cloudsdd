// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package azure

import "errors"

// ErrResourceNotSchedulable indicates that a resource explicitly declared
// a power schedule but its ResourceType has no power state (RFC 012 §3).
var ErrResourceNotSchedulable = errors.New("azure: resource type does not support a power schedule")

// ErrUnsupportedSize indicates a compute_instance size with no Azure VM
// size mapping (RFC 013 §2.1). An unmapped value is a hard error rather
// than a fallback: silently substituting a different machine is the
// failure mode an intent-driven system cannot tolerate.
var ErrUnsupportedSize = errors.New("azure: unsupported compute size")

// ErrUnsupportedOS indicates a compute_instance image with no Azure
// marketplace mapping (RFC 013 §2.1).
var ErrUnsupportedOS = errors.New("azure: unsupported operating system")
