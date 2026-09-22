// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package gcp

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"net/netip"
	"strings"

	gcpcompute "github.com/pulumi/pulumi-gcp/sdk/v7/go/gcp/compute"
	"github.com/pulumi/pulumi-gcp/sdk/v7/go/gcp/servicenetworking"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/provider"
	"cloudsdd/internal/provider/network"
	"cloudsdd/internal/spec"
)

// The service GCP peers a VPC with so that managed products — Cloud SQL
// among them — can be reached on a private address.
const (
	peeringService = "servicenetworking.googleapis.com"

	// peeringPrefixLength is the size of the range handed to Google for
	// the peered subnet its managed services live in. /24 is the smallest
	// Cloud SQL accepts; the scope's own /20 has room for it alongside
	// the workload subnet.
	peeringPrefixLength = 24

	// peeringPurpose marks the reserved range as one Google may consume.
	peeringPurpose     = "VPC_PEERING"
	peeringAddressType = "INTERNAL"
)

// Cloud NAT configuration (RFC 017 §2.7).
const (
	// natIPAllocateAuto lets Google allocate the external addresses. The
	// alternative is reserving static ones, which pins the scope's egress
	// to an address an account-level control could allow-list — worth
	// having, and worth an RFC of its own rather than a default that
	// leaves reserved addresses behind on every destroy.
	natIPAllocateAuto = "AUTO_ONLY"

	// natSubnetworksList restricts NAT to the subnets named below rather
	// than every subnet in the network. The network has exactly one today,
	// so the two settings behave identically — the explicit list is what
	// keeps them identical when a later RFC adds a second subnet that was
	// never meant to reach the internet.
	natSubnetworksList = "LIST_OF_SUBNETWORKS"
	natAllIPRanges     = "ALL_IP_RANGES"
)

// EnsureNetwork provisions the shared VPC for a scope (RFC 016 §2.2).
//
// This is the change that makes a GCP database reachable at all. Before
// it, the provider set Ipv4Enabled: false with no PrivateNetwork, which
// is not "private" so much as *absent*: Cloud SQL needs a VPC with
// private services access to have any address, so the instance came up
// with no path to it whatsoever (RFC 016 §1).
//
// The scope's contents carry the volumes its resources mount, and this
// stack owns their Filestore instances (RFC 020 §2.1).
func (p *GCPProvider) EnsureNetwork(ctx context.Context, s provider.NetworkScope, contents provider.ScopeContents, policies spec.Policies) error {
	// object_storage is global on GCP and reaches here without a region.
	// It lives in no network.
	if s.Region == "" {
		return nil
	}

	cidr, err := network.Derive(provider.NetworkScopeOf(s), provider.AddressPolicyOf(policies))
	if err != nil {
		return err
	}

	program := func(pctx *pulumi.Context) error {
		return declareScopeNetwork(pctx, s, cidr, contents)
	}

	stack, err := p.upsertStack(ctx, s.Account, s.Environment, s.Region, "", program)
	if err != nil {
		return fmt.Errorf("gcp: failed to prepare the network stack for scope %q: %w", provider.ScopeName(s), err)
	}
	if _, err := stack.Up(ctx); err != nil {
		return fmt.Errorf("gcp: failed to provision the network for scope %q: %w", provider.ScopeName(s), err)
	}
	return nil
}

// declareScopeNetwork builds the VPC, the workload subnet, the private
// services access peering without which Cloud SQL has no address, and the
// filesystems the scope's resources mount.
func declareScopeNetwork(
	ctx *pulumi.Context,
	s provider.NetworkScope,
	cidr netip.Prefix,
	contents provider.ScopeContents,
) error {
	name := scopeNetworkName(s)

	// AutoCreateSubnetworks false: the automatic mode would create a
	// subnet in every region out of a range Google picks, which is
	// exactly the overlap RFC 016 §2.4.1 exists to prevent.
	vpc, err := gcpcompute.NewNetwork(ctx, name, &gcpcompute.NetworkArgs{
		Name:                  pulumi.String(name),
		AutoCreateSubnetworks: pulumi.Bool(false),
		Description: pulumi.String(fmt.Sprintf(
			"CloudSDD network for scope %s", provider.ScopeName(s))),
	})
	if err != nil {
		return fmt.Errorf("gcp: failed to declare the network for scope %q: %w", provider.ScopeName(s), err)
	}

	workload, err := subnetBlock(cidr, 0)
	if err != nil {
		return fmt.Errorf("gcp: scope %q: %w", provider.ScopeName(s), err)
	}
	subnet, err := gcpcompute.NewSubnetwork(ctx, name+"-subnet", &gcpcompute.SubnetworkArgs{
		Name:        pulumi.String(name + "-subnet"),
		Network:     vpc.ID(),
		Region:      pulumi.String(s.Region),
		IpCidrRange: pulumi.String(workload.String()),
		// Lets an instance with no external address reach Google APIs —
		// which is how it reaches Cloud SQL's admin API and the OS Login
		// service without a route to the internet.
		PrivateIpGoogleAccess: pulumi.Bool(true),
	})
	if err != nil {
		return fmt.Errorf("gcp: failed to declare the subnet for scope %q: %w", provider.ScopeName(s), err)
	}

	if err := declareScopeEgress(ctx, s, vpc, subnet); err != nil {
		return err
	}

	// The peered range Google allocates its managed services out of. It
	// is carved from the scope's own /20 rather than left to Google to
	// choose, so the whole scope stays inside the range the address plan
	// assigned it (RFC 016 §2.4.1) — a Google-chosen range would be
	// outside it and could overlap another scope.
	peering, err := subnetBlock(cidr, 1)
	if err != nil {
		return fmt.Errorf("gcp: scope %q: %w", provider.ScopeName(s), err)
	}
	peeringRange, err := gcpcompute.NewGlobalAddress(ctx, name+"-peering", &gcpcompute.GlobalAddressArgs{
		Name:         pulumi.String(name + "-peering"),
		Purpose:      pulumi.String(peeringPurpose),
		AddressType:  pulumi.String(peeringAddressType),
		Address:      pulumi.String(peering.Addr().String()),
		PrefixLength: pulumi.Int(peeringPrefixLength),
		Network:      vpc.ID(),
	})
	if err != nil {
		return fmt.Errorf("gcp: failed to reserve the peering range for scope %q: %w", provider.ScopeName(s), err)
	}

	// Without this connection the reserved range is just a reservation:
	// Cloud SQL cannot place an instance, and Ipv4Enabled: false yields
	// an instance with no address of any kind.
	if _, err := servicenetworking.NewConnection(ctx, name+"-peering-connection",
		&servicenetworking.ConnectionArgs{
			Network:               vpc.ID(),
			Service:               pulumi.String(peeringService),
			ReservedPeeringRanges: pulumi.StringArray{peeringRange.Name},
		}); err != nil {
		return fmt.Errorf("gcp: failed to peer the network for scope %q with %s: %w",
			provider.ScopeName(s), peeringService, err)
	}

	// No firewall rules are declared here. GCP rules are allow-only and a
	// network with none denies inbound already; the priority-0 deny RFC
	// 013 added exists because the *default* network ships
	// default-allow-ssh, and this network ships nothing. compute_instance
	// still declares its own deny as defence in depth, retargeted at this
	// network.
	return declareScopeFilesystems(ctx, s, vpc, contents.Volumes)
}

// declareScopeEgress gives the scope's subnet a route out (RFC 017 §2.7).
//
// GCP expresses egress as a property of a router rather than of a subnet,
// so there is no public tier here and no address assigned to any instance:
// PrivateIpGoogleAccess already reaches Google's own APIs, and Cloud NAT
// adds everything else. An instance keeps a private address only and still
// pulls a package or an image.
//
// One NAT for the scope, which on GCP is what the API expresses anyway —
// a Cloud NAT is regional and covers every zone the subnet spans, so the
// per-AZ multiplication the AWS provider has to refuse does not arise.
func declareScopeEgress(
	ctx *pulumi.Context,
	s provider.NetworkScope,
	vpc *gcpcompute.Network,
	subnet *gcpcompute.Subnetwork,
) error {
	name := scopeNetworkName(s)

	router, err := gcpcompute.NewRouter(ctx, name+"-router", &gcpcompute.RouterArgs{
		Name:    pulumi.String(name + "-router"),
		Network: vpc.ID(),
		Region:  pulumi.String(s.Region),
		Description: pulumi.String(fmt.Sprintf(
			"CloudSDD egress router for scope %s", provider.ScopeName(s))),
	})
	if err != nil {
		return fmt.Errorf("gcp: failed to declare the router for scope %q: %w", provider.ScopeName(s), err)
	}

	if _, err := gcpcompute.NewRouterNat(ctx, name+"-nat", &gcpcompute.RouterNatArgs{
		Name:                          pulumi.String(name + "-nat"),
		Router:                        router.Name,
		Region:                        pulumi.String(s.Region),
		NatIpAllocateOption:           pulumi.String(natIPAllocateAuto),
		SourceSubnetworkIpRangesToNat: pulumi.String(natSubnetworksList),
		Subnetworks: gcpcompute.RouterNatSubnetworkArray{
			&gcpcompute.RouterNatSubnetworkArgs{
				Name:                 subnet.ID(),
				SourceIpRangesToNats: pulumi.StringArray{pulumi.String(natAllIPRanges)},
			},
		},
	}); err != nil {
		return fmt.Errorf("gcp: failed to declare the nat for scope %q: %w", provider.ScopeName(s), err)
	}
	return nil
}

// subnetBlock carves the index'th /24 out of the scope's range.
//
// Whole-address arithmetic rather than an octet increment, for the reason
// the AWS provider records: incrementing an octet is correct only while
// the scope is a /16 or smaller, and wraps silently otherwise.
func subnetBlock(cidr netip.Prefix, index int) (netip.Prefix, error) {
	const subnetBits = 24

	if !cidr.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("scope range %s is not IPv4", cidr)
	}
	if cidr.Bits() > subnetBits {
		return netip.Prefix{}, fmt.Errorf("scope range %s is smaller than a /%d subnet", cidr, subnetBits)
	}
	available := uint64(1) << (subnetBits - cidr.Bits())
	if index < 0 || uint64(index) >= available {
		return netip.Prefix{}, fmt.Errorf("scope range %s holds %d subnets, asked for index %d",
			cidr, available, index)
	}

	baseAddr := cidr.Addr().As4()
	start := uint64(binary.BigEndian.Uint32(baseAddr[:]))
	addr := start + uint64(index)*(1<<(32-subnetBits))
	if addr > math.MaxUint32 {
		return netip.Prefix{}, fmt.Errorf("subnet %d does not fit in %s", index, cidr)
	}

	var raw [4]byte
	binary.BigEndian.PutUint32(raw[:], uint32(addr))
	return netip.PrefixFrom(netip.AddrFrom4(raw), subnetBits), nil
}

// scopeNetworkName is the VPC's name, derived from the scope.
//
// GCP resource names are unique within a project and addressable by name,
// so unlike AWS this provider needs no tag lookup and no invoke: a
// resource program names the network it wants and GCP resolves it. The
// name must start with a letter, hold only lowercase letters, digits and
// hyphens, and fit in 63 characters.
func scopeNetworkName(s provider.NetworkScope) string {
	var b strings.Builder
	b.WriteString("cloudsdd-")
	for _, r := range strings.ToLower(provider.ScopeName(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}

	name := b.String()
	if len(name) > 63 {
		// Truncating risks a collision between two long scopes, so the
		// tail is replaced by a hash of the whole name rather than
		// dropped. Distinct scopes keep distinct networks, which is the
		// property that matters.
		name = name[:54] + "-" + provider.ShortHash(name)
	}
	return strings.TrimRight(name, "-")
}

// DestroyNetwork removes the scope's network stack (RFC 016 §2.6).
//
// The Engine establishes that the scope is empty before calling this.
// Nothing here can check: a scope's contents live in other stacks, and
// this one knows only about the network.
func (p *GCPProvider) DestroyNetwork(ctx context.Context, s provider.NetworkScope, contents provider.ScopeContents, policies spec.Policies) error {
	if s.Region == "" {
		return nil
	}

	cidr, err := network.Derive(provider.NetworkScopeOf(s), provider.AddressPolicyOf(policies))
	if err != nil {
		return err
	}

	program := func(pctx *pulumi.Context) error {
		return declareScopeNetwork(pctx, s, cidr, contents)
	}
	stack, err := p.upsertStack(ctx, s.Account, s.Environment, s.Region, "", program)
	if err != nil {
		return fmt.Errorf("gcp: failed to select the network stack for scope %q: %w", provider.ScopeName(s), err)
	}
	if _, err := stack.Destroy(ctx); err != nil {
		return fmt.Errorf("gcp: failed to destroy the network for scope %q: %w", provider.ScopeName(s), err)
	}
	return nil
}
