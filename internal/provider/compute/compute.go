// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

// Package compute holds the cloud-agnostic shape of a compute_instance
// resource: the properties every provider decodes, and the placement rules
// they all apply (RFC 013 §2.1, §2.3).
//
// The struct lives here rather than being redeclared per provider for the
// reason RFC 011 §1.1H recorded: three copies of a schema drift, and a
// drifted schema means a Specification that is valid on one cloud and
// silently different on another. Each provider still decodes through its
// own decode.Decoder, so error prefixes and provider-specific validator
// tags stay where they belong; only the shape is shared.
//
// What is deliberately *not* shared is the mapping from these values onto
// SKUs and images. That is per-provider by nature, and each package owns
// its own table.
package compute

import (
	"errors"
	"fmt"
)

// Size is the cloud-agnostic instance size. The user says how much
// machine they need; each provider picks the SKU that expresses it, the
// same way nobody writes "db.t3.micro" in a Specification today.
type Size string

const (
	SizeSmall  Size = "small"
	SizeMedium Size = "medium"
	SizeLarge  Size = "large"
)

// OS is the cloud-agnostic operating system image.
//
// The enum holds only images that exist on all three clouds. Amazon Linux
// and Azure's own images are excluded on purpose: including them would
// make this field's validity depend on Resource.Provider, which is the
// kind of hidden coupling the schema avoids (RFC 013 §7.2).
type OS string

const (
	OSUbuntu2204 OS = "ubuntu-22.04"
	OSUbuntu2404 OS = "ubuntu-24.04"
	OSDebian12   OS = "debian-12"
)

// defaultDiskSizeGB is the root volume size when none is requested. Every
// supported image boots comfortably in it.
const defaultDiskSizeGB = 20

// Properties represents the Properties of a Resource with Type:
// "compute_instance" on any provider (RFC 013 §2.1).
//
// PublicIP is *bool, not bool, so its absence activates the secure default
// rather than the zero value — the tri-state pattern RFC 011 §2.5
// established for every security-relevant property.
//
// There is deliberately no SSH key field: access goes through each cloud's
// IAM-authenticated session service (RFC 013 §2.4), and a key in the
// Specification would be a long-lived credential in a document RFC 001 §3
// treats as hostile input.
type Properties struct {
	Size Size `json:"size" validate:"required,oneof=small medium large"`
	OS   OS   `json:"os" validate:"required,oneof=ubuntu-22.04 ubuntu-24.04 debian-12"`

	// DiskSizeGB is the root volume size in gigabytes.
	DiskSizeGB int `json:"disk_size_gb,omitempty" validate:"omitempty,min=8,max=1024"`

	// PublicIP attaches a public address. It opens no inbound port: the
	// instance ships with a default-deny firewall either way, so a public
	// address alone leaves it unreachable.
	PublicIP *bool `json:"public_ip,omitempty"`
}

// EffectiveDiskSizeGB returns DiskSizeGB, or the default when absent.
func (p Properties) EffectiveDiskSizeGB() int {
	if p.DiskSizeGB == 0 {
		return defaultDiskSizeGB
	}
	return p.DiskSizeGB
}

// EffectivePublicIP reports whether a public address was explicitly
// requested. Absent means false: private by default, like every other
// resource type in this schema.
func (p Properties) EffectivePublicIP() bool {
	return p.PublicIP != nil && *p.PublicIP
}

// ErrMultipleZones indicates that more than one availability zone was
// requested for a single instance (RFC 013 §2.3).
//
// A virtual machine lives in exactly one zone. Quietly taking the first
// entry would hand back infrastructure that does not match what was
// written, which is the one failure mode an intent-driven system cannot
// tolerate. Spreading across zones needs an instance group, a resource
// type this schema does not yet have.
var ErrMultipleZones = errors.New("compute_instance occupies a single availability zone")

// Zone resolves Scope.Zones for a single instance: the empty string when
// none was requested, meaning the provider lets the cloud choose.
func Zone(zones []string) (string, error) {
	switch len(zones) {
	case 0:
		return "", nil
	case 1:
		return zones[0], nil
	default:
		return "", fmt.Errorf("%w: %d requested", ErrMultipleZones, len(zones))
	}
}
