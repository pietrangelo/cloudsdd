// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package network

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
)

// TestDeriveIsGolden pins the derivation for a fixed set of scopes.
//
// This is the most load-bearing test in the package and the least
// interesting to read. A range that has been deployed cannot be
// recomputed without rebuilding the network and everything in it, so a
// change to the hash — a different algorithm, a different separator, a
// different slot size — must break a test rather than silently
// re-address every network in existence. If this test fails, the
// question is never "update the expectations"; it is "what did we just
// change, and who is already running on the old answer?"
func TestDeriveIsGolden(t *testing.T) {
	tests := []struct {
		scope Scope
		want  string
	}{
		{Scope{}, "10.177.64.0/20"},
		{Scope{Environment: "dev"}, "10.144.0.0/20"},
		{Scope{Environment: "prod"}, "10.32.144.0/20"},
		{Scope{Account: "prod", Environment: "live", Region: "eu-central-1"}, "10.231.192.0/20"},
		{Scope{Account: "prod", Environment: "live", Region: "eu-west-1"}, "10.79.112.0/20"},
		{Scope{Account: "staging", Environment: "live", Region: "eu-central-1"}, "10.235.208.0/20"},
		{Scope{Environment: "dev", Region: "europe-west1"}, "10.5.160.0/20"},
		{Scope{Environment: "dev", Region: "westeurope"}, "10.46.16.0/20"},
	}

	for _, tt := range tests {
		t.Run(tt.scope.hashKey(), func(t *testing.T) {
			got, err := Derive(tt.scope, Policy{})
			if err != nil {
				t.Fatalf("Derive() error = %v", err)
			}
			if got.String() != tt.want {
				t.Errorf("Derive(%q) = %s, want %s — see this test's doc comment before changing it",
					tt.scope, got, tt.want)
			}
		})
	}
}

// TestDeriveIsDeterministic covers the property the golden test pins,
// stated directly: the same scope always yields the same range. CloudSDD
// keeps its state in a local file, so two engineers deploying two
// environments from two machines cannot coordinate through it.
// Determinism is what lets them not need to.
func TestDeriveIsDeterministic(t *testing.T) {
	s := Scope{Account: "prod", Environment: "live", Region: "eu-central-1"}

	first, err := Derive(s, Policy{})
	if err != nil {
		t.Fatalf("Derive() error = %v", err)
	}
	for i := 0; i < 100; i++ {
		again, err := Derive(s, Policy{})
		if err != nil {
			t.Fatalf("Derive() error = %v", err)
		}
		if again != first {
			t.Fatalf("Derive() returned %s then %s for the same scope", first, again)
		}
	}
}

// TestScopeSegmentsAreUnambiguous is why the tuple is joined with a
// separator that cannot occur in a segment. Were it joined naively,
// ("a", "bc") and ("ab", "c") would hash identically and two distinct
// scopes would share one network.
func TestScopeSegmentsAreUnambiguous(t *testing.T) {
	a, err := Derive(Scope{Account: "a", Environment: "bc"}, Policy{})
	if err != nil {
		t.Fatalf("Derive() error = %v", err)
	}
	b, err := Derive(Scope{Account: "ab", Environment: "c"}, Policy{})
	if err != nil {
		t.Fatalf("Derive() error = %v", err)
	}
	if a == b {
		t.Errorf("("+`"a","bc"`+") and ("+`"ab","c"`+") both derived %s", a)
	}
}

// TestEmptySegmentsAreDistinct covers the other half of the same
// concern: an empty segment carries meaning, so it must not be dropped
// before hashing the way Scope.String drops it for display.
func TestEmptySegmentsAreDistinct(t *testing.T) {
	withAccount, err := Derive(Scope{Account: "dev"}, Policy{})
	if err != nil {
		t.Fatalf("Derive() error = %v", err)
	}
	withEnv, err := Derive(Scope{Environment: "dev"}, Policy{})
	if err != nil {
		t.Fatalf("Derive() error = %v", err)
	}
	if withAccount == withEnv {
		t.Errorf("account %q and environment %q derived the same range %s", "dev", "dev", withAccount)
	}
}

// realisticScopes is a scope matrix of the shape a working installation
// produces: several environments across several regions in one account.
func realisticScopes(account string) []Scope {
	var out []Scope
	for _, env := range []string{"dev", "test", "staging", "prod", "sandbox", "qa"} {
		for _, region := range []string{
			"eu-central-1", "eu-west-1", "us-east-1", "europe-west1", "westeurope",
		} {
			out = append(out, Scope{Account: account, Environment: env, Region: region})
		}
	}
	return out
}

// TestDistinctScopesGetDistinctRanges is the guarantee the whole package
// exists to provide (RFC 016 §2.4.1). Overlapping ranges are harmless
// until the day somebody peers two networks, at which point the operation
// is impossible and the only fix is rebuilding both.
func TestDistinctScopesGetDistinctRanges(t *testing.T) {
	scopes := realisticScopes("prod")

	seen := make(map[netip.Prefix]Scope, len(scopes))
	for _, s := range scopes {
		got, err := Derive(s, Policy{})
		if err != nil {
			t.Fatalf("Derive(%q) error = %v", s, err)
		}
		if other, clash := seen[got]; clash {
			t.Errorf("scopes %q and %q both derived %s", other, s, got)
		}
		seen[got] = s
	}

	if err := Check(scopes, Policy{}); err != nil {
		t.Errorf("Check() error = %v, want no conflict across a realistic scope matrix", err)
	}
}

// TestAccountsArePartitioned pins RFC 016 §2.1. Two accounts holding the
// same range is the *correct* outcome, not a collision: CloudSDD creates
// no route between accounts, so there is nothing to conflict. A refactor
// of Check that started comparing across accounts would fail here rather
// than in an operator's console, and a warning that fires on correct
// behaviour is how real warnings come to be ignored.
func TestAccountsArePartitioned(t *testing.T) {
	// Same environment and region, different accounts: by construction
	// these derive different ranges, so force the collision with pins to
	// test what Check does about it rather than what the hash happens to
	// produce.
	policy := Policy{Scopes: map[string]string{
		"prod-a::live::eu-central-1": "10.50.0.0/20",
		"prod-b::live::eu-central-1": "10.50.0.0/20",
	}}
	scopes := []Scope{
		{Account: "prod-a", Environment: "live", Region: "eu-central-1"},
		{Account: "prod-b", Environment: "live", Region: "eu-central-1"},
	}

	if err := Check(scopes, policy); err != nil {
		t.Errorf("Check() error = %v, want identical ranges in different accounts to be accepted", err)
	}
}

// TestCollisionIsRefusedNotResolved mirrors RFC 014's ErrAmbiguousProvider
// test: an ambiguity the system cannot settle correctly is reported, never
// broken by a guess.
func TestCollisionIsRefusedNotResolved(t *testing.T) {
	policy := Policy{Scopes: map[string]string{
		"prod::dev::eu-central-1":  "10.50.0.0/20",
		"prod::test::eu-central-1": "10.50.0.0/20",
	}}
	scopes := []Scope{
		{Account: "prod", Environment: "dev", Region: "eu-central-1"},
		{Account: "prod", Environment: "test", Region: "eu-central-1"},
	}

	err := Check(scopes, policy)
	if err == nil {
		t.Fatal("Check() error = nil, want the overlap to be refused")
	}

	var conflict Conflict
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %T, want a Conflict", err)
	}
	// The message has to name both scopes and the way out, or it sends
	// the operator looking through three providers for a range nobody
	// printed.
	for _, want := range []string{"prod::dev::eu-central-1", "prod::test::eu-central-1", "policies.network.scopes"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestCheckIsDeterministic guards the sorted iteration. Ranging over a
// map would name a different pair of scopes on different runs of an
// unchanged Specification, which makes a reported conflict impossible to
// discuss.
func TestCheckIsDeterministic(t *testing.T) {
	policy := Policy{Scopes: map[string]string{
		"prod::a::eu-central-1": "10.50.0.0/20",
		"prod::b::eu-central-1": "10.50.0.0/20",
		"prod::c::eu-central-1": "10.50.0.0/20",
	}}
	scopes := []Scope{
		{Account: "prod", Environment: "a", Region: "eu-central-1"},
		{Account: "prod", Environment: "b", Region: "eu-central-1"},
		{Account: "prod", Environment: "c", Region: "eu-central-1"},
	}

	first := Check(scopes, policy)
	if first == nil {
		t.Fatal("Check() error = nil, want a conflict")
	}
	for i := 0; i < 50; i++ {
		if got := Check(scopes, policy); got.Error() != first.Error() {
			t.Fatalf("Check() reported %q then %q", first, got)
		}
	}
}

func TestBaseCIDR(t *testing.T) {
	tests := []struct {
		name    string
		base    string
		wantErr string
	}{
		{name: "default when empty"},
		{name: "10/8 accepted", base: "10.0.0.0/8"},
		{name: "172.16/12 accepted", base: "172.16.0.0/12"},
		{name: "192.168/16 accepted", base: "192.168.0.0/16"},
		{name: "exactly one slot accepted", base: "10.1.0.0/20"},
		{name: "public range refused", base: "8.8.0.0/16", wantErr: "RFC 1918"},
		{name: "too small for a slot", base: "10.1.0.0/24", wantErr: "smaller than one"},
		{name: "malformed", base: "not-a-cidr", wantErr: "invalid base_cidr"},
		{name: "ipv6 refused", base: "fd00::/8", wantErr: "must be IPv4"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Derive(Scope{Environment: "dev"}, Policy{BaseCIDR: tt.base})

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Derive() error = nil, want %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("Derive() error = %v", err)
			}
			if got.Bits() != SlotBits {
				t.Errorf("derived %s, want a /%d", got, SlotBits)
			}

			base := tt.base
			if base == "" {
				base = DefaultBaseCIDR
			}
			if !netip.MustParsePrefix(base).Overlaps(got) {
				t.Errorf("derived %s, which is outside base_cidr %s", got, base)
			}
		})
	}
}

// TestPinsAreValidatedLikeDerivedRanges covers RFC 016 §2.4.2. An
// operator-supplied range is at least as likely to collide as a computed
// one, so accepting it unchecked would make the escape hatch the least
// safe path through the system.
func TestPinsAreValidatedLikeDerivedRanges(t *testing.T) {
	const key = "prod::live::eu-central-1"
	scope := Scope{Account: "prod", Environment: "live", Region: "eu-central-1"}

	tests := []struct {
		name    string
		base    string
		pin     string
		wantErr string
	}{
		{name: "valid pin inside the base", pin: "10.50.0.0/20"},
		{name: "valid pin in a custom base", base: "172.20.0.0/14", pin: "172.20.16.0/20"},
		{name: "outside the base", base: "172.20.0.0/14", pin: "10.50.0.0/20", wantErr: "outside base_cidr"},
		{name: "public range", pin: "8.8.8.0/24", wantErr: "RFC 1918"},
		{name: "malformed", pin: "10.50.0.0/nope", wantErr: "invalid cidr"},
		{name: "wider than the base", base: "172.20.0.0/14", pin: "172.16.0.0/12", wantErr: "outside base_cidr"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := Policy{BaseCIDR: tt.base, Scopes: map[string]string{key: tt.pin}}

			got, err := Derive(scope, policy)

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Derive() error = nil, want %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("Derive() error = %v", err)
			}
			if got.String() != netip.MustParsePrefix(tt.pin).String() {
				t.Errorf("Derive() = %s, want the pinned %s", got, tt.pin)
			}
		})
	}
}

func TestScopeString(t *testing.T) {
	tests := []struct {
		scope Scope
		want  string
	}{
		{Scope{Account: "prod", Environment: "live", Region: "eu-central-1"}, "prod::live::eu-central-1"},
		{Scope{Environment: "dev", Region: "eu-central-1"}, "dev::eu-central-1"},
		{Scope{Region: "eu-central-1"}, "eu-central-1"},
		{Scope{}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.scope.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

// FuzzDerive asserts the invariants that must hold for every scope,
// including ones no test author would think to write: the range is always
// a /20, always inside the base block, and always correctly aligned.
// A misaligned or out-of-range prefix would be rejected by the cloud at
// apply time, i.e. after the user approved a plan.
func FuzzDerive(f *testing.F) {
	f.Add("", "", "")
	f.Add("prod", "live", "eu-central-1")
	f.Add("::", "::", "::")
	f.Add("\x00", "\xff\xfe", "a")

	base := netip.MustParsePrefix(DefaultBaseCIDR)

	f.Fuzz(func(t *testing.T, account, environment, region string) {
		s := Scope{Account: account, Environment: environment, Region: region}

		got, err := Derive(s, Policy{})
		if err != nil {
			t.Fatalf("Derive(%q) error = %v; derivation must succeed for every scope", s, err)
		}
		if got.Bits() != SlotBits {
			t.Errorf("Derive(%q) = %s, want a /%d", s, got, SlotBits)
		}
		if !base.Overlaps(got) {
			t.Errorf("Derive(%q) = %s, outside %s", s, got, base)
		}
		if got.Masked() != got {
			t.Errorf("Derive(%q) = %s, which is not aligned to its own prefix", s, got)
		}
	})
}
