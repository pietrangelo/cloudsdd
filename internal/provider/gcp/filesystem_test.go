// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package gcp

import (
	"net/netip"
	"regexp"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/provider"
	"cloudsdd/internal/spec"
)

const filestoreInstanceToken = "gcp:filestore/instance:Instance"

// shareNamePattern is what Filestore accepts for a file share: at most
// sixteen characters, a letter first, then letters, digits and
// underscores. No hyphen — the one character every other GCP name here is
// built from.
var shareNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,15}$`)

// testScopeContents is what the Engine hands the network stack for a scope
// whose resources mount two filesystems (RFC 020 §2.3).
//
// The sizes differ on purpose, so that an instance given the other
// volume's capacity is caught. Both are at or above the 1 TiB floor
// because Validate refuses anything less before a program ever runs (RFC
// 020 §2.7); an absent size cannot reach this stack on GCP.
func testScopeContents() provider.ScopeContents {
	return provider.ScopeContents{Volumes: []spec.Volume{
		{Name: "uploads", SizeGB: 1024},
		{Name: "cache", SizeGB: 2048},
	}}
}

// TestDeclareScopeFilesystems covers the Filestore mapping of RFC 020 §2.6:
// one BASIC_HDD instance per volume, sized as asked, reachable only on a
// private address inside the scope's own network, and pinned to one zone.
func TestDeclareScopeFilesystems(t *testing.T) {
	cidr := netip.MustParsePrefix("10.42.0.0/20")
	contents := testScopeContents()

	recorded := runProgram(t, func(ctx *pulumi.Context) error {
		return declareScopeNetwork(ctx, testScope(), cidr, contents)
	})

	instances := resourcesOfType(recorded, filestoreInstanceToken)
	if len(instances) != len(contents.Volumes) {
		t.Fatalf("declared %d Filestore instances, want one per volume (%d)",
			len(instances), len(contents.Volumes))
	}

	for _, v := range contents.Volumes {
		want := filestoreInstanceNameFor(testScope(), v.Name)
		instance, ok := instanceNamed(instances, want)
		if !ok {
			t.Fatalf("volume %q: no Filestore instance named %q; the mounting stack derives that name "+
				"and would find nothing", v.Name, want)
		}

		// The cheapest tier that exists. Anything above it is a pricing
		// decision the Specification has no field to make.
		if got := instance.Inputs["tier"].StringValue(); got != "BASIC_HDD" {
			t.Errorf("volume %q: tier = %q, want BASIC_HDD", v.Name, got)
		}

		// Basic tiers are zonal, so the zone is chosen, and chosen the same
		// way every time: a zone picked any other way would move the
		// instance, and a moved instance is a replaced one.
		if got := instance.Inputs["location"].StringValue(); got != "europe-west1-a" {
			t.Errorf("volume %q: location = %q, want the region's first zone europe-west1-a", v.Name, got)
		}

		// Without the scope in its description an operator sees an instance
		// whose name ends in a hash and cannot tell which environment owns it.
		description := instance.Inputs["description"].StringValue()
		if !strings.Contains(description, provider.ScopeName(testScope())) || !strings.Contains(description, v.Name) {
			t.Errorf("volume %q: description = %q, want it to name the volume and the scope %q",
				v.Name, description, provider.ScopeName(testScope()))
		}

		assertFileShare(t, v, instance)
		assertFilestoreNetwork(t, v, instance)
	}
}

// assertFileShare pins the one share each instance carries: sized from the
// volume's own size_gb, under the name the mounting stack will ask for.
func assertFileShare(t *testing.T, v spec.Volume, instance recordedResource) {
	t.Helper()

	share := instance.Inputs["fileShares"].ObjectValue()
	if got := share["capacityGb"].NumberValue(); got != float64(v.SizeGB) {
		t.Errorf("volume %q: capacityGb = %v, want the requested %d", v.Name, got, v.SizeGB)
	}

	name := share["name"].StringValue()
	if name != filestoreShareName {
		t.Errorf("volume %q: share name = %q, want %q — the mount reads %q", v.Name, name,
			filestoreShareName, filestoreShareName)
	}
	if !shareNamePattern.MatchString(filestoreShareName) {
		t.Errorf("share name %q is not one Filestore accepts: at most 16 characters, a letter "+
			"first, letters, digits and underscores only", filestoreShareName)
	}
}

// assertFilestoreNetwork pins how the instance is reached: a private IPv4
// address peered straight into the scope's VPC and nowhere else.
//
// reservedIpRange is asserted absent rather than merely "not wrong". A
// range fixed here would have to be carved from the scope's /20 by an
// index, and that index renumbers when a volume is added — a renumbered
// instance is a replaced instance, and a replaced instance is lost data.
func assertFilestoreNetwork(t *testing.T, v spec.Volume, instance recordedResource) {
	t.Helper()

	networks := instance.Inputs["networks"].ArrayValue()
	if len(networks) != 1 {
		t.Fatalf("volume %q: attached to %d networks, want only the scope VPC", v.Name, len(networks))
	}
	n := networks[0].ObjectValue()

	if got := n["network"].StringValue(); got != scopeNetworkName(testScope()) {
		t.Errorf("volume %q: network = %q, want the scope VPC %q", v.Name, got, scopeNetworkName(testScope()))
	}
	if got := n["connectMode"].StringValue(); got != "DIRECT_PEERING" {
		t.Errorf("volume %q: connectMode = %q, want DIRECT_PEERING", v.Name, got)
	}
	modes := n["modes"].ArrayValue()
	if len(modes) != 1 || modes[0].StringValue() != "MODE_IPV4" {
		t.Errorf("volume %q: modes = %v, want only MODE_IPV4", v.Name, modes)
	}
	if reserved, set := n["reservedIpRange"]; set && !reserved.IsNull() {
		t.Errorf("volume %q: reservedIpRange = %v, want it left to the service", v.Name, reserved)
	}
}

// TestDeclareScopeNetworkWithoutVolumesDeclaresNoFilesystem is the half of
// the contract that keeps every existing scope unchanged: Filestore's floor
// is about two hundred dollars a month, and a scope that mounts nothing
// must not pay it.
func TestDeclareScopeNetworkWithoutVolumesDeclaresNoFilesystem(t *testing.T) {
	cidr := netip.MustParsePrefix("10.42.0.0/20")

	recorded := runProgram(t, func(ctx *pulumi.Context) error {
		return declareScopeNetwork(ctx, testScope(), cidr, provider.ScopeContents{})
	})

	if n := len(resourcesOfType(recorded, filestoreInstanceToken)); n != 0 {
		t.Errorf("declared %d Filestore instances with no volumes in the scope, want none", n)
	}
}

// TestFilestoreInstanceNameIsDerivable is what lets two stacks agree on an
// instance without talking to each other: the network stack names it and
// the mounting stack recomputes the name from the same scope and volume
// (RFC 020 §2.5).
//
// The value is pinned literally, not recomputed through ShortHash. The name
// is the identity of a live instance; a formula that drifts renames it, and
// on Filestore a rename is a replacement.
func TestFilestoreInstanceNameIsDerivable(t *testing.T) {
	got := filestoreInstanceNameFor(testScope(), "uploads")
	if want := "cloudsdd-fs-uploads-e68f08bd"; got != want {
		t.Errorf("filestoreInstanceNameFor() = %q, want %q", got, want)
	}
}

// TestFilestoreInstanceNameIsAValidAndDistinctName attacks the derivation
// with what the Specification admits and GCP does not: uppercase,
// underscores, a leading digit, a trailing hyphen, the full 32 characters.
//
// Every one must come out as a name GCP accepts, and no two may come out
// the same. The pairs are the ones a fold to GCP's charset would merge —
// "Cache" and "cache" are two volumes in the Specification, and one
// instance for both would be two services silently sharing a filesystem
// neither of them asked to share.
func TestFilestoreInstanceNameIsAValidAndDistinctName(t *testing.T) {
	other := provider.NetworkScope{Provider: spec.ProviderGCP, Environment: "prod", Region: "europe-west1", Sealed: true}
	long := provider.NetworkScope{
		Account:     strings.Repeat("a", 32),
		Environment: strings.Repeat("e", 32),
		Region:      "europe-west1",
	}

	type input struct {
		scope  provider.NetworkScope
		volume string
	}
	inputs := []input{
		{testScope(), "cache"},
		{testScope(), "Cache"},
		{testScope(), "a_b"},
		{testScope(), "a-b"},
		{testScope(), "a"},
		{testScope(), "a-"},
		{testScope(), "9lives"},
		{testScope(), strings.Repeat("Z", 32)},
		{other, "cache"},
		{long, strings.Repeat("v_", 16)},
	}

	seen := make(map[string]input, len(inputs))
	for _, in := range inputs {
		name := filestoreInstanceNameFor(in.scope, in.volume)
		assertValidGCPName(t, name)

		if prior, taken := seen[name]; taken {
			t.Errorf("volume %q in scope %q and volume %q in scope %q both derive %q",
				prior.volume, provider.ScopeName(prior.scope), in.volume, provider.ScopeName(in.scope), name)
		}
		seen[name] = in

		if again := filestoreInstanceNameFor(in.scope, in.volume); again != name {
			t.Errorf("volume %q derived %q then %q; the two stacks would disagree", in.volume, name, again)
		}
	}
}

func instanceNamed(instances []recordedResource, name string) (recordedResource, bool) {
	for _, r := range instances {
		if r.Inputs["name"].StringValue() == name {
			return r, true
		}
	}
	return recordedResource{}, false
}
