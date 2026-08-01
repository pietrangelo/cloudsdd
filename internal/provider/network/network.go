// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

// Package network decides the address range of every CloudSDD-created
// network (RFC 016 §2.4.1).
//
// It is pure: no clock, no environment, no I/O, no cloud. Every decision
// about addresses in CloudSDD is made here, for the same reason every
// decision about time is made in internal/schedule — a rule that lives in
// one testable place is a rule that can be reasoned about, and one spread
// across three providers is three rules that will disagree.
//
// The problem being solved is not "pick a range". Any range works until
// something needs to cross between two of them; then overlapping ranges
// make peering, VPN attachment and region-joining impossible, and the only
// remedy is rebuilding the network and everything inside it. Allocating
// carelessly here means writing a migration for the operator to perform
// later, so allocation is deterministic and checked rather than
// convenient.
package network

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"net/netip"
	"sort"
	"strings"
)

// ScopeSeparator joins the segments of a scope before hashing.
//
// The same separator RFC 005 §2.6 chose for stack names, and for the same
// reason: it cannot occur in an account name, an environment name or a
// region string, so ("a", "bc") and ("ab", "c") can never hash to the
// same slot. An ambiguous join would hand two different scopes one range.
const ScopeSeparator = "::"

// Address plan defaults (RFC 016 §2.4.1).
const (
	// DefaultBaseCIDR is the block scopes are carved out of when the
	// operator states no plan of their own.
	DefaultBaseCIDR = "10.0.0.0/8"

	// SlotBits is the prefix length of one scope's network. A /20 is
	// 4096 addresses — enough for a multi-AZ network with room spare —
	// and yields 4096 slots inside a /8. Slots are the scarce resource
	// here, not addresses within a slot: a /16 per scope would leave
	// only 256 slots, which a hash collides in far too readily.
	SlotBits = 20
)

// Scope is the boundary one network serves (RFC 016 §2.1).
//
// Account is part of the identity, not decoration: two accounts deriving
// the same range is the correct outcome, never a collision, because
// CloudSDD creates no route between accounts.
type Scope struct {
	Account     string
	Environment string
	Region      string
}

// String renders the scope the way stack names render it, which is also
// the key format used by Policy.Scopes.
func (s Scope) String() string {
	parts := make([]string, 0, 3)
	for _, p := range []string{s.Account, s.Environment, s.Region} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, ScopeSeparator)
}

// hashKey is the exact string fed to the hash.
//
// Unlike String it keeps empty segments, because an empty segment is
// meaningful: the scope (account "", environment "dev") is a different
// network from (account "dev", environment ""), and dropping empties
// would collapse them onto one range.
func (s Scope) hashKey() string {
	return strings.Join([]string{s.Account, s.Environment, s.Region}, ScopeSeparator)
}

// Policy is the operator's address plan (RFC 016 §2.4.2). The zero value
// is valid and means "derive everything from DefaultBaseCIDR".
type Policy struct {
	// BaseCIDR confines derivation to the block the operator set aside
	// for CloudSDD. Empty means DefaultBaseCIDR.
	BaseCIDR string

	// Scopes pins individual scopes, keyed by Scope.String(). It is the
	// escape hatch when derivation collides, and the way to honour a
	// corporate address plan that assigns ranges by hand.
	Scopes map[string]string
}

// Derive returns the address range for a scope.
//
// A pin in Policy.Scopes wins; otherwise the range is the slot whose
// index is the scope's hash. The hash is sha256 truncated to 64 bits —
// a fixed algorithm, never a seeded or per-process one, because changing
// it re-addresses every network that has ever been deployed. That is why
// there is a golden test.
func Derive(s Scope, p Policy) (netip.Prefix, error) {
	base, err := baseBlock(p)
	if err != nil {
		return netip.Prefix{}, err
	}

	if pinned, ok := p.Scopes[s.String()]; ok {
		return parsePin(s, pinned, base)
	}

	slots := uint64(1) << (SlotBits - base.Bits())
	sum := sha256.Sum256([]byte(s.hashKey()))
	index := binary.BigEndian.Uint64(sum[:8]) % slots

	prefix, err := slotAt(base, index)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("network: scope %q: %w", s, err)
	}
	return prefix, nil
}

// Conflict reports two distinct scopes in one account that resolved to
// overlapping ranges.
type Conflict struct {
	A, B   Scope
	RangeA netip.Prefix
	RangeB netip.Prefix
}

func (c Conflict) Error() string {
	return fmt.Sprintf(
		"network: scopes %q (%s) and %q (%s) overlap in account %q.\n"+
			"Pin one of them under policies.network.scopes to resolve it",
		c.A, c.RangeA, c.B, c.RangeB, c.A.Account)
}

// Check derives a range for every scope and reports the first overlap
// between two distinct scopes *in the same account*.
//
// Refusing rather than resolving is RFC 014's stance applied to
// addresses: an ambiguity the system cannot settle correctly is reported,
// never broken by a guess. A silently overlapping network is a defect
// that surfaces months later, during an operation that then cannot be
// completed at all.
//
// Scopes in different accounts are never compared. Two accounts holding
// the same range is the correct outcome of RFC 016 §2.1 — CloudSDD
// creates no route between accounts — and flagging it would be a warning
// that fires on correct behaviour, which is how real warnings come to be
// ignored.
func Check(scopes []Scope, p Policy) error {
	ranges := make(map[Scope]netip.Prefix, len(scopes))
	for _, s := range scopes {
		if _, seen := ranges[s]; seen {
			continue
		}
		r, err := Derive(s, p)
		if err != nil {
			return err
		}
		ranges[s] = r
	}

	// Sorted, so the conflict reported for a given input is always the
	// same one: ranging over a map would name a different pair on
	// different runs of an unchanged Specification.
	ordered := make([]Scope, 0, len(ranges))
	for s := range ranges {
		ordered = append(ordered, s)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].hashKey() < ordered[j].hashKey() })

	for i := 0; i < len(ordered); i++ {
		for j := i + 1; j < len(ordered); j++ {
			a, b := ordered[i], ordered[j]
			if a.Account != b.Account {
				continue
			}
			if overlaps(ranges[a], ranges[b]) {
				return Conflict{A: a, B: b, RangeA: ranges[a], RangeB: ranges[b]}
			}
		}
	}
	return nil
}

// baseBlock parses and validates Policy.BaseCIDR.
func baseBlock(p Policy) (netip.Prefix, error) {
	raw := p.BaseCIDR
	if raw == "" {
		raw = DefaultBaseCIDR
	}

	base, err := netip.ParsePrefix(raw)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("network: invalid base_cidr %q: %w", raw, err)
	}
	base = base.Masked()

	if !base.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("network: base_cidr %q must be IPv4", raw)
	}
	if !isPrivate(base.Addr()) {
		return netip.Prefix{}, fmt.Errorf(
			"network: base_cidr %q must be a private RFC 1918 range (10/8, 172.16/12, 192.168/16)", raw)
	}
	if base.Bits() > SlotBits {
		return netip.Prefix{}, fmt.Errorf(
			"network: base_cidr %q is smaller than one /%d scope network", raw, SlotBits)
	}
	return base, nil
}

// parsePin validates an operator-supplied range as strictly as a derived
// one: a hand-written range is at least as likely to collide as a
// computed one, and accepting it unchecked would make the escape hatch
// the least safe path through the system.
func parsePin(s Scope, raw string, base netip.Prefix) (netip.Prefix, error) {
	pin, err := netip.ParsePrefix(raw)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("network: scope %q: invalid cidr %q: %w", s, raw, err)
	}
	pin = pin.Masked()

	if !pin.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("network: scope %q: cidr %q must be IPv4", s, raw)
	}
	if !isPrivate(pin.Addr()) {
		return netip.Prefix{}, fmt.Errorf(
			"network: scope %q: cidr %q must be a private RFC 1918 range", s, raw)
	}
	if !base.Overlaps(pin) || pin.Bits() < base.Bits() {
		return netip.Prefix{}, fmt.Errorf(
			"network: scope %q: cidr %q is outside base_cidr %s", s, raw, base)
	}
	return pin, nil
}

// slotAt returns the index'th block of SlotBits inside base.
//
// The arithmetic is done in uint64 and narrowed once, after the bound
// check, rather than added in uint32 and hoped about: an address that
// wrapped would silently name a range in somebody else's network instead
// of failing.
func slotAt(base netip.Prefix, index uint64) (netip.Prefix, error) {
	baseAddr := base.Addr().As4()
	start := uint64(binary.BigEndian.Uint32(baseAddr[:]))
	size := uint64(1) << (32 - SlotBits)

	// Unreachable with the validated inputs — baseBlock caps the prefix
	// at SlotBits, so index < slots and the sum always fits. Kept because
	// the alternative to an impossible error is an undetected wrap.
	last := start + index*size
	if last > math.MaxUint32 {
		return netip.Prefix{}, fmt.Errorf("slot %d does not fit in %s", index, base)
	}

	var raw [4]byte
	binary.BigEndian.PutUint32(raw[:], uint32(last))
	return netip.PrefixFrom(netip.AddrFrom4(raw), SlotBits), nil
}

// overlaps reports whether two prefixes share any address.
func overlaps(a, b netip.Prefix) bool { return a.Overlaps(b) }

// rfc1918 is the set of ranges a CloudSDD network may occupy. A public
// range here would have CloudSDD hand out addresses it does not own, and
// silently blackhole the real hosts using them.
var rfc1918 = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
}

func isPrivate(addr netip.Addr) bool {
	for _, p := range rfc1918 {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
