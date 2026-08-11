// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package aws

import (
	"encoding/json"
	"fmt"

	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/ec2"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/efs"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/provider"
	"cloudsdd/internal/spec"
)

// The EFS posture RFC 020 §2.6 specifies for AWS.
const (
	// nfsPort is the only port a mount target answers on.
	nfsPort = 2049

	// nonRootUID is the POSIX identity the access point pins every request
	// to. A container that mounts as root can write over the whole
	// filesystem regardless of what the image runs as; 1000 is the first
	// ordinary user on every distribution CloudSDD is likely to meet.
	nonRootUID = 1000

	// rootDirectoryMode is what the access point creates its per-volume
	// directory with: readable by all, writable only by the owner the
	// access point already pins.
	rootDirectoryMode = "0755"

	// transitionToIA moves files nobody has read in a month to infrequent
	// access. It is the cost control of §2.6 — without it a filesystem
	// untouched for a year still bills at the standard rate — and it is
	// invisible to a reader: EFS pulls a file back on access.
	transitionToIA = "AFTER_30_DAYS"

	// maxCreationToken is what EFS accepts for a creation token, counted in
	// characters. Beyond it the API refuses the filesystem at apply, after
	// the user approved a plan that looked fine.
	maxCreationToken = 64

	// tokenBodyLimit is what is left of that limit once the collision
	// suffix is accounted for: eight characters of provider.ShortHash and
	// the hyphen before it.
	tokenBodyLimit = maxCreationToken - 9
)

// fileSystemTokenFor is the EFS creation token of a scope's volume.
//
// It is the mechanism dbSubnetGroupNameFor already uses, for the same
// reason: the filesystem is created by the network stack and mounted from
// a resource stack, so the two have to agree on a name without talking to
// each other. Both derive it from (scope, volume name) by arithmetic.
//
// It deliberately does not reuse dbSubnetGroupNameFor. That name is folded
// to the charset RDS accepts; a creation token is free-form up to 64
// characters, so folding would be lossy for nothing — and case-folding
// would be actively wrong, because a volume name is case-sensitive in the
// Specification and "Cache" and "cache" must stay two filesystems. Sharing
// the derivation would also mean a change to RDS naming renames every
// filesystem, and on EFS a rename is a replacement.
func fileSystemTokenFor(s provider.NetworkScope, volume string) string {
	token := "cloudsdd::" + scopeTag(s) + "::" + volume

	// Counted in runes because AWS states the limit in characters, and
	// truncated rather than dropped: two long scopes whose difference falls
	// past the cut would otherwise share one filesystem, which is the
	// silent wrong answer this RFC exists to prevent.
	runes := []rune(token)
	if len(runes) <= maxCreationToken {
		return token
	}
	return string(runes[:tokenBodyLimit]) + "-" + provider.ShortHash(token)
}

// declareScopeFilesystems declares one EFS filesystem per volume the scope
// mounts, plus the single security group that governs who may reach them
// (RFC 020 §2.5, §2.6).
//
// A scope whose resources mount nothing pays for nothing: no volumes means
// no filesystems and no security group either.
func declareScopeFilesystems(
	ctx *pulumi.Context,
	s provider.NetworkScope,
	vpc *ec2.Vpc,
	cidr string,
	private []*ec2.Subnet,
	volumes []spec.Volume,
	opts ...pulumi.ResourceOption,
) error {
	if len(volumes) == 0 {
		return nil
	}

	group, err := declareFilesystemSecurityGroup(ctx, s, vpc, cidr, opts...)
	if err != nil {
		return err
	}
	for _, v := range volumes {
		if err := declareScopeFilesystem(ctx, s, v, group, private, opts...); err != nil {
			return err
		}
	}
	return nil
}

// declareFilesystemSecurityGroup is what may reach the mount targets: NFS
// from the scope's own range, and nothing else.
//
// One group for every filesystem in the scope rather than one each. They
// are reachable by exactly the same set of clients — anything inside the
// VPC — so a group per filesystem would be N copies of one decision, and
// N places for it to drift.
func declareFilesystemSecurityGroup(
	ctx *pulumi.Context,
	s provider.NetworkScope,
	vpc *ec2.Vpc,
	cidr string,
	opts ...pulumi.ResourceOption,
) (*ec2.SecurityGroup, error) {
	group, err := ec2.NewSecurityGroup(ctx, "cloudsdd-net-fs-sg", &ec2.SecurityGroupArgs{
		VpcId:       vpc.ID(),
		Description: pulumi.String(fmt.Sprintf("CloudSDD %s: nfs from the scope network only", scopeTag(s))),
		Ingress: ec2.SecurityGroupIngressArray{
			&ec2.SecurityGroupIngressArgs{
				Protocol: pulumi.String(protocolTCP),
				FromPort: pulumi.Int(nfsPort),
				ToPort:   pulumi.Int(nfsPort),
				// By CIDR rather than by the mounting service's group: the
				// filesystem outlives every resource that mounts it, so it
				// cannot name one. The range is the scope's own /20, which
				// has no route in from the internet.
				CidrBlocks: pulumi.StringArray{pulumi.String(cidr)},
			},
		},
		// Empty on purpose, and empty rather than absent: a mount target
		// answers connections and opens none, so any egress rule here would
		// grant reach nothing uses.
		Egress: ec2.SecurityGroupEgressArray{},
		Tags:   scopeTags(s, nil),
	}, opts...)
	if err != nil {
		return nil, fmt.Errorf("aws: failed to declare the filesystem security group for scope %q: %w",
			scopeTag(s), err)
	}
	return group, nil
}

// declareScopeFilesystem declares one volume's filesystem: the filesystem
// itself, a mount target in every private subnet, the access point a
// container reaches it through, the resource policy governing how, and the
// backup that survives the scope (RFC 020 §2.6).
func declareScopeFilesystem(
	ctx *pulumi.Context,
	s provider.NetworkScope,
	v spec.Volume,
	group *ec2.SecurityGroup,
	private []*ec2.Subnet,
	opts ...pulumi.ResourceOption,
) error {
	name := "cloudsdd-net-fs-" + v.Name

	// SizeGB is not read here, and that is the whole of §2.7's AWS row: EFS
	// capacity is elastic, so there is no size to request and a value
	// invented from the field would be discarded by the API anyway.
	fileSystem, err := efs.NewFileSystem(ctx, name, &efs.FileSystemArgs{
		// The name both stacks derive rather than one Pulumi generates: the
		// mounting stack has no way to see a random suffix.
		CreationToken: pulumi.String(fileSystemTokenFor(s, v.Name)),
		Encrypted:     pulumi.Bool(true),
		LifecyclePolicies: efs.FileSystemLifecyclePolicyArray{
			&efs.FileSystemLifecyclePolicyArgs{
				TransitionToIa: pulumi.String(transitionToIA),
			},
		},
		// The Name tag is what an operator reads in the console. Without it
		// two filesystems are told apart only by a generated ID, and the
		// creation token has to be decoded to learn which share is which.
		Tags: scopeTags(s, map[string]string{"Name": v.Name}),
	}, opts...)
	if err != nil {
		return fmt.Errorf("aws: failed to declare the filesystem for volume %q in scope %q: %w",
			v.Name, scopeTag(s), err)
	}

	// One mount target per private subnet. EFS permits exactly one per
	// availability zone and the private tier is one subnet per zone, so the
	// two line up without a special case — and a zone with no mount target
	// is a zone whose tasks cannot mount at all.
	for i, subnet := range private {
		if _, err := efs.NewMountTarget(ctx, fmt.Sprintf("%s-mt-%d", name, i), &efs.MountTargetArgs{
			FileSystemId:   fileSystem.ID(),
			SubnetId:       subnet.ID(),
			SecurityGroups: pulumi.StringArray{group.ID()},
		}, opts...); err != nil {
			return fmt.Errorf("aws: failed to declare mount target %d for volume %q in scope %q: %w",
				i, v.Name, scopeTag(s), err)
		}
	}

	// The access point is the least-privilege half: every request through
	// it is made as one ordinary user against one directory, whatever the
	// image runs as. creationInfo is not optional in practice — the
	// directory does not exist on a filesystem created empty, and without
	// it the mount fails at task start rather than at apply.
	if _, err := efs.NewAccessPoint(ctx, name+"-ap", &efs.AccessPointArgs{
		FileSystemId: fileSystem.ID(),
		PosixUser: &efs.AccessPointPosixUserArgs{
			Uid: pulumi.Int(nonRootUID),
			Gid: pulumi.Int(nonRootUID),
		},
		RootDirectory: &efs.AccessPointRootDirectoryArgs{
			Path: pulumi.String("/" + v.Name),
			CreationInfo: &efs.AccessPointRootDirectoryCreationInfoArgs{
				OwnerUid:    pulumi.Int(nonRootUID),
				OwnerGid:    pulumi.Int(nonRootUID),
				Permissions: pulumi.String(rootDirectoryMode),
			},
		},
		Tags: scopeTags(s, map[string]string{"Name": v.Name}),
	}, opts...); err != nil {
		return fmt.Errorf("aws: failed to declare the access point for volume %q in scope %q: %w",
			v.Name, scopeTag(s), err)
	}

	document, err := fileSystemPolicyDocument(v.Name)
	if err != nil {
		return err
	}
	if _, err := efs.NewFileSystemPolicy(ctx, name+"-policy", &efs.FileSystemPolicyArgs{
		FileSystemId: fileSystem.ID(),
		Policy:       pulumi.String(document),
	}, opts...); err != nil {
		return fmt.Errorf("aws: failed to declare the file system policy for volume %q in scope %q: %w",
			v.Name, scopeTag(s), err)
	}

	// A destroy takes the filesystem with the scope (§2.9), so this is the
	// only thing standing between a reaped environment and lost data.
	if _, err := efs.NewBackupPolicy(ctx, name+"-backup", &efs.BackupPolicyArgs{
		FileSystemId: fileSystem.ID(),
		BackupPolicy: &efs.BackupPolicyBackupPolicyArgs{
			Status: pulumi.String("ENABLED"),
		},
	}, opts...); err != nil {
		return fmt.Errorf("aws: failed to declare the backup policy for volume %q in scope %q: %w",
			v.Name, scopeTag(s), err)
	}
	return nil
}

// fileSystemPolicyDocument is the resource policy every CloudSDD
// filesystem carries (RFC 020 §2.6).
//
// Two statements answering two different attackers. The allow's condition
// means a stolen credential cannot reach the filesystem over the EFS API
// from outside the VPC — only a client coming through a mount target,
// which exists only in the scope's private subnets, is admitted. The deny
// means a client already inside cannot fall back to plaintext NFS.
//
// Neither grants elasticfilesystem:ClientRootAccess, which is the one
// permission that would make the access point's non-root user pointless.
//
// There is no Resource element: a file system policy is attached to the
// one filesystem it governs, so AWS applies it there, and naming the ARN
// would mean threading an Output through a document that is otherwise a
// pure function of the volume.
func fileSystemPolicyDocument(volume string) (string, error) {
	// Principal is "*" and the condition does the narrowing. The principals
	// are the task roles of resources that do not exist yet and are created
	// in other stacks, so the filesystem cannot enumerate them; what it can
	// state is that they must arrive through its own mount targets.
	anyPrincipal := map[string]string{"AWS": "*"}

	policy := map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{
			{
				"Sid":       "AllowMountThroughMountTargetOnly",
				"Effect":    "Allow",
				"Principal": anyPrincipal,
				"Action": []string{
					"elasticfilesystem:ClientMount",
					"elasticfilesystem:ClientWrite",
				},
				"Condition": map[string]any{
					"Bool": map[string]string{"elasticfilesystem:AccessedViaMountTarget": "true"},
				},
			},
			{
				"Sid":       "DenyUnencryptedTransport",
				"Effect":    "Deny",
				"Principal": anyPrincipal,
				"Action":    "*",
				"Condition": map[string]any{
					"Bool": map[string]string{"aws:SecureTransport": "false"},
				},
			},
		},
	}

	encoded, err := json.Marshal(policy)
	if err != nil {
		return "", fmt.Errorf("aws: failed to render the file system policy for volume %q: %w", volume, err)
	}
	return string(encoded), nil
}

// mountedFilesystem is one of a resource's volumes resolved to the scope
// filesystem it names: everything a task definition and a task role need
// to reach it, and nothing else.
type mountedFilesystem struct {
	volume spec.Volume

	// fileSystemID and accessPointID are what the task definition mounts
	// through. The access point is not optional: without it the task
	// reaches the whole filesystem as root, whatever the image runs as.
	fileSystemID  string
	accessPointID string

	// fileSystemARN is the single resource the task role's mount grant
	// names, which is what keeps that grant from being a wildcard.
	fileSystemARN string
}

// lookupMountedFilesystems finds the scope filesystem behind each of a
// resource's volumes (RFC 020 §2.8).
//
// It is the mirror of declareScopeFilesystems: the network stack created
// these, this stack only finds them, and the two meet at the creation
// token fileSystemTokenFor derives for both. It creates nothing — a
// filesystem declared here would be a second one beside the scope's, and
// the services meant to share a volume would each see an empty directory.
func lookupMountedFilesystems(
	ctx *pulumi.Context,
	s provider.NetworkScope,
	volumes []spec.Volume,
) ([]mountedFilesystem, error) {
	mounts := make([]mountedFilesystem, 0, len(volumes))
	for _, v := range volumes {
		token := fileSystemTokenFor(s, v.Name)

		fileSystem, err := efs.LookupFileSystem(ctx, &efs.LookupFileSystemArgs{CreationToken: &token}, nil)
		if err != nil {
			return nil, fmt.Errorf(
				"%w: volume %q in scope %q has no filesystem under creation token %q. The scope's "+
					"network is provisioned before the resources in it, so this means it was removed "+
					"out of band: %w", ErrFilesystemMissing, v.Name, scopeTag(s), token, err)
		}

		// The access point the network stack gave that filesystem, sought by
		// filesystem because AWS generates its ID and no stack can derive
		// one. Exactly one exists per filesystem, so the set is a set of one.
		points, err := efs.GetAccessPoints(ctx, &efs.GetAccessPointsArgs{
			FileSystemId: fileSystem.FileSystemId,
		}, nil)
		if err != nil {
			return nil, fmt.Errorf("aws: failed to look up the access point of volume %q in scope %q: %w",
				v.Name, scopeTag(s), err)
		}
		// Not "take the first": a filesystem carrying two access points is
		// one this stack cannot choose between, and choosing anyway would
		// mount a task somewhere nobody asked for.
		if len(points.Ids) != 1 {
			return nil, fmt.Errorf(
				"%w: the filesystem for volume %q in scope %q carries %d access points, want the one "+
					"its network stack declares. A task mounts through the access point, so mounting "+
					"without it would fail at task start rather than here",
				ErrFilesystemMissing, v.Name, scopeTag(s), len(points.Ids))
		}

		mounts = append(mounts, mountedFilesystem{
			volume:        v,
			fileSystemID:  fileSystem.FileSystemId,
			accessPointID: points.Ids[0],
			fileSystemARN: fileSystem.Arn,
		})
	}
	return mounts, nil
}
