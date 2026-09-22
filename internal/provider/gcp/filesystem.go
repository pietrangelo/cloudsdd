// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package gcp

import (
	"fmt"
	"strings"

	gcpcompute "github.com/pulumi/pulumi-gcp/sdk/v7/go/gcp/compute"
	"github.com/pulumi/pulumi-gcp/sdk/v7/go/gcp/filestore"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/provider"
	"cloudsdd/internal/spec"
)

// The Filestore posture RFC 020 §2.6 specifies for GCP.
const (
	// filestoreTier is the cheapest tier that exists. Anything above it is
	// a pricing decision the Specification has no field to make.
	filestoreTier = "BASIC_HDD"

	// filestoreShareName is the one share every instance carries. The
	// instance name already identifies the volume, so the share needs no
	// identity of its own — and Filestore's sixteen-character, underscore
	// only charset could not hold one derived from a volume name anyway.
	filestoreShareName = "data"

	// DIRECT_PEERING peers the instance straight into the scope's VPC, on
	// a range the service chooses. Nothing outside the VPC has a route to
	// it; there is no public address to disable because none exists.
	filestoreConnectMode = "DIRECT_PEERING"
	filestoreAddressMode = "MODE_IPV4"
)

// filestoreInstanceNameFor is the Filestore instance name of a scope's
// volume.
//
// The network stack creates the instance and a resource stack mounts it,
// so the two agree on a name by deriving it from (scope, volume name)
// rather than by talking to each other (RFC 020 §2.5).
//
// The hash is appended always, not only on overflow, and is taken over
// the unfolded label. A volume name admits uppercase and underscores and
// GCP does not, so the readable part is lossy — "Cache" and "cache" fold
// to one — and the hash is what keeps them two instances. It also bounds
// the length without a truncation branch: the prefix, a 32-character
// volume, a hyphen and eight hex characters stay under 63. The scope lives
// in the description instead, where an operator reads it.
func filestoreInstanceNameFor(s provider.NetworkScope, volume string) string {
	var b strings.Builder
	b.WriteString("cloudsdd-fs-")
	for _, r := range strings.ToLower(volume) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String() + "-" + provider.ShortHash(provider.ScopeName(s)+"::"+volume)
}

// declareScopeFilesystems declares one Filestore instance per volume the
// scope mounts (RFC 020 §2.6).
//
// A scope whose resources mount nothing pays for nothing: Filestore's
// floor is a terabyte, and no volumes means no instance.
func declareScopeFilesystems(
	ctx *pulumi.Context,
	s provider.NetworkScope,
	vpc *gcpcompute.Network,
	volumes []spec.Volume,
) error {
	for _, v := range volumes {
		if err := declareScopeFilesystem(ctx, s, vpc, v); err != nil {
			return err
		}
	}
	return nil
}

// declareScopeFilesystem is one volume's instance: reachable only on a
// private address inside the scope's VPC, and pinned to one zone.
//
// reservedIpRange is left to the service on purpose. A range fixed here
// would be carved from the scope's /20 by an index that renumbers when a
// volume is added, and a renumbered instance is a replaced one — which on
// a filesystem is lost data.
func declareScopeFilesystem(
	ctx *pulumi.Context,
	s provider.NetworkScope,
	vpc *gcpcompute.Network,
	v spec.Volume,
) error {
	name := filestoreInstanceNameFor(s, v.Name)

	if _, err := filestore.NewInstance(ctx, name, &filestore.InstanceArgs{
		Name: pulumi.String(name),
		Description: pulumi.String(fmt.Sprintf(
			"CloudSDD volume %s for scope %s", v.Name, provider.ScopeName(s))),
		Tier: pulumi.String(filestoreTier),
		// Basic tiers are zonal. The zone is chosen the same way every time,
		// because a zone chosen any other way moves the instance.
		Location: pulumi.String(instanceZone(s.Region, "")),
		FileShares: &filestore.InstanceFileSharesArgs{
			Name:       pulumi.String(filestoreShareName),
			CapacityGb: pulumi.Int(v.SizeGB),
		},
		Networks: filestore.InstanceNetworkArray{
			&filestore.InstanceNetworkArgs{
				Network:     vpc.Name,
				ConnectMode: pulumi.String(filestoreConnectMode),
				Modes:       pulumi.StringArray{pulumi.String(filestoreAddressMode)},
			},
		},
	}); err != nil {
		return fmt.Errorf("gcp: failed to declare the filesystem for volume %q in scope %q: %w",
			v.Name, provider.ScopeName(s), err)
	}
	return nil
}

// mountedFilesystem is a volume paired with the instance the scope's
// network stack built for it: everything a Cloud Run template needs to
// mount it.
type mountedFilesystem struct {
	volume   spec.Volume
	instance string
	server   string
}

// lookupMountedFilesystems finds the scope instance behind each of a
// resource's volumes (RFC 020 §2.8).
//
// It is the mirror of declareScopeFilesystems: the network stack created
// these, this stack only finds them, and the two meet at the name
// filestoreInstanceNameFor derives for both. It creates nothing — an
// instance declared here would be a second one beside the scope's, and the
// services meant to share a volume would each see an empty directory.
func lookupMountedFilesystems(
	ctx *pulumi.Context,
	s provider.NetworkScope,
	volumes []spec.Volume,
) ([]mountedFilesystem, error) {
	// Basic tiers are zonal, so the lookup names the zone the instance was
	// placed in; the provider's default location would find nothing.
	zone := instanceZone(s.Region, "")

	mounts := make([]mountedFilesystem, 0, len(volumes))
	for _, v := range volumes {
		name := filestoreInstanceNameFor(s, v.Name)

		instance, err := filestore.LookupInstance(ctx, &filestore.LookupInstanceArgs{
			Name:     name,
			Location: &zone,
		})
		if err != nil {
			return nil, fmt.Errorf(
				"%w: volume %q in scope %q has no Filestore instance named %q in %s. The scope's "+
					"network is provisioned before the resources in it, so this means it was removed "+
					"out of band: %w", ErrFilesystemMissing, v.Name, provider.ScopeName(s), name, zone, err)
		}

		server, ok := privateAddress(instance)
		if !ok {
			return nil, fmt.Errorf(
				"%w: the Filestore instance %q for volume %q in scope %q reports no private address. "+
					"An NFS volume with no server is a template Cloud Run accepts and a revision that "+
					"fails at start, so it is refused here instead",
				ErrFilesystemMissing, name, v.Name, provider.ScopeName(s))
		}

		mounts = append(mounts, mountedFilesystem{volume: v, instance: name, server: server})
	}
	return mounts, nil
}

// privateAddress is the address an instance serves NFS on. The network
// stack joins each instance to exactly one network, in MODE_IPV4, so the
// first address of the first network is the only one there is.
func privateAddress(instance *filestore.LookupInstanceResult) (string, bool) {
	if len(instance.Networks) == 0 || len(instance.Networks[0].IpAddresses) == 0 {
		return "", false
	}
	return instance.Networks[0].IpAddresses[0], true
}
