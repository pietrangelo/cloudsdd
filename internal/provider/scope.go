// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package provider

import (
	"crypto/sha256"
	"encoding/hex"

	"cloudsdd/internal/provider/network"
	"cloudsdd/internal/spec"
)

// The scope helpers every provider needs (RFC 019 §2.4).
//
// NetworkScopeOf and AddressPolicyOf are constructors for
// internal/provider/network's own types, which cannot live there: they
// read provider.NetworkScope and spec.Policies, and that package is a
// leaf that must import neither (RFC 019 §2.1). They live here instead,
// where NetworkScope is already defined and spec is already imported.

// ResourceScope is the network scope a resource belongs to.
//
// The provider is a parameter because it is the one dimension the
// resource does not carry, and the coarsest partition of all: an AWS
// account and an Azure subscription can never share a network. Zones and
// the sealing posture are deliberately absent — a network spans the whole
// region, and Sealed governs what may be peered to it rather than which
// resources join it, so carrying either would make one scope look like
// two.
func ResourceScope(p spec.Provider, r spec.Resource) NetworkScope {
	return NetworkScope{
		Provider:    p,
		Account:     r.Account,
		Environment: r.Scope.Environment,
		Region:      r.Scope.Region,
	}
}

// NetworkScopeOf projects a provider scope onto the address model.
//
// The provider is dropped: it belongs to the network's identity but not
// to its address. Two clouds deriving the same range is the correct
// outcome, because CloudSDD creates no route between them — and were the
// provider in the key, moving a workload from AWS to GCP would silently
// re-address it.
func NetworkScopeOf(s NetworkScope) network.Scope {
	return network.Scope{
		Account:     s.Account,
		Environment: s.Environment,
		Region:      s.Region,
	}
}

// AddressPolicyOf translates the Specification's network policy for the
// address package, which stays free of spec types. An absent plan means
// "derive everything from the default block", so it maps to the zero
// Policy rather than to an error.
func AddressPolicyOf(p spec.Policies) network.Policy {
	if p.Network == nil {
		return network.Policy{}
	}
	return network.Policy{BaseCIDR: p.Network.BaseCIDR, Scopes: p.Network.Scopes}
}

// ScopeName labels a scope for resource names and for every error message
// about it. An unscoped deployment is legal (RFC 016 §7.4), so it reads as
// "default" rather than as an empty pair of quotes.
func ScopeName(s NetworkScope) string {
	label := NetworkScopeOf(s).String()
	if label == "" {
		return "default"
	}
	return label
}

// ShortHash is a stable 8-character tag used to keep two long scope names
// from colliding after truncation.
//
// GCP and Azure both cut a scope label to fit a length limit and append
// this tag, which makes it part of resource names that already exist in
// real infrastructure. Changing the algorithm renames every one of them —
// a replacement, not an update — so sha256 truncated to four bytes is a
// fixed decision rather than an implementation detail free to drift.
func ShortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:4])
}
