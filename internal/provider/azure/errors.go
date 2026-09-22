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

// ErrZonesNotSupported indicates that Scope.Zones was set on a
// ResourceType with no zone-aware placement (RFC 005 §2.5).
//
// Refused rather than ignored: a zone forwarded and dropped is a placement
// the user asked for and did not get.
var ErrZonesNotSupported = errors.New("azure: resource type does not support scope.zones")

// ErrUnsupportedContainerSize indicates a container_service size with no
// Container Apps CPU/memory pair (RFC 017 §2.2).
//
// Separate from ErrUnsupportedSize, which is compute_instance's: the two
// share a vocabulary and nothing else, and one error would make a message
// about VM SKUs appear for a container.
var ErrUnsupportedContainerSize = errors.New("azure: unsupported container size")

// ErrFilesystemMissing indicates that a resource mounts a volume whose
// storage account or share is not in its scope (RFC 020 §2.8).
//
// The Engine provisions a scope's network stack — which owns the account
// and its shares — before any resource in it, so this is a broken
// invariant rather than a race: something was removed out of band.
// Creating a replacement instead would be RFC 020 §1's silent wrong
// answer: a second, empty share where a shared one was meant to be.
var ErrFilesystemMissing = errors.New("azure: the scope does not hold the filesystem a resource mounts")
