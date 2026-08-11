// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package aws

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/provider"
	"cloudsdd/internal/spec"
)

const (
	fileSystemToken       = "aws:efs/fileSystem:FileSystem"
	fileSystemPolicyToken = "aws:efs/fileSystemPolicy:FileSystemPolicy"
	mountTargetToken      = "aws:efs/mountTarget:MountTarget"
	accessPointToken      = "aws:efs/accessPoint:AccessPoint"
	efsBackupPolicyToken  = "aws:efs/backupPolicy:BackupPolicy"

	// The two invokes a mounting resource stack makes (RFC 020 §2.8). It
	// creates nothing: everything above belongs to the scope's network
	// stack, and these are how this stack finds it.
	getFileSystemToken   = "aws:efs/getFileSystem:getFileSystem"
	getAccessPointsToken = "aws:efs/getAccessPoints:getAccessPoints"
)

// testFileSystemID is the identity the mock monitor hands back for a
// looked-up filesystem.
//
// Derived from the creation token rather than fixed, so two volumes never
// look like one filesystem: it is what lets the mount assertions tie each
// volume to its own filesystem and its own access point instead of merely
// counting them.
func testFileSystemID(creationToken string) string {
	return "fs-" + provider.ShortHash(creationToken)
}

func testFileSystemARN(fileSystemID string) string {
	return "arn:aws:elasticfilesystem:eu-central-1:" + testAccountID + ":file-system/" + fileSystemID
}

// testAccessPointID is the one access point the network stack gives each
// filesystem (RFC 020 §2.6), in the mock's terms.
func testAccessPointID(fileSystemID string) string {
	return "fsap-" + strings.TrimPrefix(fileSystemID, "fs-")
}

// testScopeContents is what the Engine hands the network stack for a scope
// whose resources mount two filesystems (RFC 020 §2.3).
//
// Neither volume carries a MountPath, and that is not an omission: the
// merged record deliberately drops it, because a path is where a
// filesystem appears inside one container and the scope's copy is shared
// by all of them. "uploads" carries a size and "cache" does not, which on
// AWS must make no difference at all — EFS capacity is elastic and §2.7
// says the field is ignored here.
func testScopeContents() provider.ScopeContents {
	return provider.ScopeContents{Volumes: []spec.Volume{
		{Name: "uploads", SizeGB: 100},
		{Name: "cache"},
	}}
}

// mockID is the ID the mock monitor hands back for a declared resource.
// Following its convention here is what lets the assertions below tie a
// mount target to the filesystem it serves and to the subnet it sits in,
// rather than merely counting them.
func mockID(r recordedResource) string { return r.Name + "-id" }

// TestDeclareScopeFilesystems covers the EFS posture of RFC 020 §2.6: one
// filesystem per volume, encrypted, reachable only from inside the scope's
// own network, and recoverable after the destroy of §2.9.
func TestDeclareScopeFilesystems(t *testing.T) {
	cidr := netip.MustParsePrefix("10.42.0.0/20")
	contents := testScopeContents()

	recorded := runProgram(t, func(ctx *pulumi.Context) error {
		return declareScopeNetwork(ctx, testScope(), cidr, contents)
	})

	filesystems := resourcesOfType(recorded, fileSystemToken)
	if len(filesystems) != len(contents.Volumes) {
		t.Fatalf("declared %d filesystems, want one per volume (%d)",
			len(filesystems), len(contents.Volumes))
	}

	// Every filesystem is reached from the volume it belongs to, through
	// the creation token the mounting stack will recompute. A filesystem
	// under any other token is one the resource stack can never find.
	volumeOf := make(map[string]string, len(filesystems))
	for _, v := range contents.Volumes {
		fs, ok := filesystemFor(filesystems, fileSystemTokenFor(testScope(), v.Name))
		if !ok {
			t.Fatalf("no filesystem with creation token %q; tokens = %v",
				fileSystemTokenFor(testScope(), v.Name), creationTokens(filesystems))
		}
		volumeOf[mockID(fs)] = v.Name

		if !fs.Inputs["encrypted"].BoolValue() {
			t.Errorf("volume %q: encrypted = false; the data is at rest in the clear", v.Name)
		}
		// The cost control of §2.6. Without it a filesystem nobody has
		// read in a year still bills at the standard rate.
		lifecycle := fs.Inputs["lifecyclePolicies"].ArrayValue()
		if len(lifecycle) != 1 {
			t.Fatalf("volume %q: %d lifecycle policies, want the one IA transition", v.Name, len(lifecycle))
		}
		if got := lifecycle[0].ObjectValue()["transitionToIa"].StringValue(); got != "AFTER_30_DAYS" {
			t.Errorf("volume %q: transitionToIa = %q, want AFTER_30_DAYS", v.Name, got)
		}

		tags := fs.Inputs["tags"].ObjectValue()
		if got := tags[tagScope].StringValue(); got != "dev::eu-central-1" {
			t.Errorf("volume %q: %s tag = %q, want the scope label", v.Name, tagScope, got)
		}
		if got := tags[tagManagedBy].StringValue(); got != managedByValue {
			t.Errorf("volume %q: %s tag = %q, want %q", v.Name, tagManagedBy, got, managedByValue)
		}
		// Without it the console shows two filesystems distinguished only
		// by a generated ID, and an operator cannot tell which share is
		// which without decoding a creation token.
		if got := tags["Name"].StringValue(); got != v.Name {
			t.Errorf("volume %q: Name tag = %q, want the volume name", v.Name, got)
		}
	}

	// A destroy takes the filesystems with the scope (§2.9), so the backup
	// is the only thing standing between a reaped environment and lost
	// data.
	backups := resourcesOfType(recorded, efsBackupPolicyToken)
	if len(backups) != len(contents.Volumes) {
		t.Fatalf("declared %d backup policies, want one per filesystem (%d)",
			len(backups), len(contents.Volumes))
	}
	for _, b := range backups {
		status := b.Inputs["backupPolicy"].ObjectValue()["status"].StringValue()
		if status != "ENABLED" {
			t.Errorf("backup policy for %q = %q, want ENABLED",
				volumeOf[b.Inputs["fileSystemId"].StringValue()], status)
		}
	}

	assertFilesystemSecurityGroup(t, recorded, cidr.String())
	assertMountTargets(t, recorded, volumeOf)
	assertAccessPoints(t, recorded, volumeOf)
	assertFilesystemPolicies(t, recorded, volumeOf)
}

// assertFilesystemSecurityGroup pins what may reach the mount targets.
//
// One group for the scope's filesystems, admitting NFS from the scope's
// own range and nothing else. This is what makes "private by default"
// structural rather than incidental: the mount targets sit in private
// subnets with no route in from the internet, and this rule means that
// even a future subnet with a route out could not widen who may mount.
func assertFilesystemSecurityGroup(t *testing.T, recorded []recordedResource, cidr string) {
	t.Helper()

	groups := resourcesOfType(recorded, securityGroupToken)
	if len(groups) != 1 {
		t.Fatalf("declared %d security groups in the network stack, want the one for the filesystems", len(groups))
	}
	sg := groups[0]

	if got := sg.Inputs["vpcId"].StringValue(); got != mockID(findResource(t, recorded, vpcToken)) {
		t.Errorf("filesystem security group vpcId = %q, want the scope VPC", got)
	}

	rules := sg.Inputs["ingress"].ArrayValue()
	if len(rules) != 1 {
		t.Fatalf("filesystem security group has %d ingress rules, want only NFS from the VPC", len(rules))
	}
	rule := rules[0].ObjectValue()
	if got := rule["fromPort"].NumberValue(); got != 2049 {
		t.Errorf("ingress fromPort = %v, want 2049", got)
	}
	if got := rule["toPort"].NumberValue(); got != 2049 {
		t.Errorf("ingress toPort = %v, want 2049", got)
	}
	if got := rule["protocol"].StringValue(); got != protocolTCP {
		t.Errorf("ingress protocol = %q, want %q", got, protocolTCP)
	}
	blocks := rule["cidrBlocks"].ArrayValue()
	if len(blocks) != 1 || blocks[0].StringValue() != cidr {
		t.Errorf("ingress cidrBlocks = %v, want only the scope range %q", blocks, cidr)
	}

	// A mount target never opens a connection of its own; it answers them.
	// An egress rule here would grant reach the filesystem has no use for.
	if egress := sg.Inputs["egress"].ArrayValue(); len(egress) != 0 {
		t.Errorf("filesystem security group has %d egress rules, want none", len(egress))
	}
}

// assertMountTargets covers the arithmetic of §2.6: one mount target per
// private subnet, per filesystem.
//
// EFS permits exactly one mount target per availability zone and the
// private tier is one subnet per zone, so the two line up without a
// special case. A mount target in the public tier would be an NFS endpoint
// one route table away from the internet gateway; a missing one is an
// availability zone whose tasks cannot mount at all.
func assertMountTargets(t *testing.T, recorded []recordedResource, volumeOf map[string]string) {
	t.Helper()

	private := subnetIDsByTier(recorded, tierPrivate)
	if len(private) != subnetCount {
		t.Fatalf("the network has %d private subnets, want %d", len(private), subnetCount)
	}
	sg := mockID(resourcesOfType(recorded, securityGroupToken)[0])

	targets := resourcesOfType(recorded, mountTargetToken)
	if want := len(volumeOf) * subnetCount; len(targets) != want {
		t.Fatalf("declared %d mount targets, want %d — one per private subnet per filesystem",
			len(targets), want)
	}

	subnetsOf := make(map[string]map[string]bool, len(volumeOf))
	for _, mt := range targets {
		volume := volumeOf[mt.Inputs["fileSystemId"].StringValue()]
		if volume == "" {
			t.Fatalf("mount target %q serves an unknown filesystem %q",
				mt.Name, mt.Inputs["fileSystemId"].StringValue())
		}

		subnet := mt.Inputs["subnetId"].StringValue()
		if !private[subnet] {
			t.Errorf("volume %q has a mount target in %q, which is not a private subnet", volume, subnet)
		}
		if subnetsOf[volume] == nil {
			subnetsOf[volume] = map[string]bool{}
		}
		if subnetsOf[volume][subnet] {
			t.Errorf("volume %q has two mount targets in subnet %q; EFS permits one per zone", volume, subnet)
		}
		subnetsOf[volume][subnet] = true

		groups := mt.Inputs["securityGroups"].ArrayValue()
		if len(groups) != 1 || groups[0].StringValue() != sg {
			t.Errorf("volume %q: mount target security groups = %v, want only the filesystem group",
				volume, groups)
		}
	}

	for _, volume := range volumeOf {
		if got := len(subnetsOf[volume]); got != subnetCount {
			t.Errorf("volume %q is mounted in %d private subnets, want %d", volume, got, subnetCount)
		}
	}
}

// assertAccessPoints covers the least-privilege half of §2.6: a container
// reaches its own directory as an ordinary user, not the whole filesystem
// as root.
func assertAccessPoints(t *testing.T, recorded []recordedResource, volumeOf map[string]string) {
	t.Helper()

	points := resourcesOfType(recorded, accessPointToken)
	if len(points) != len(volumeOf) {
		t.Fatalf("declared %d access points, want one per filesystem (%d)", len(points), len(volumeOf))
	}

	for _, ap := range points {
		volume := volumeOf[ap.Inputs["fileSystemId"].StringValue()]
		if volume == "" {
			t.Fatalf("access point %q belongs to an unknown filesystem", ap.Name)
		}

		user := ap.Inputs["posixUser"].ObjectValue()
		if uid := user["uid"].NumberValue(); uid != nonRootUID {
			t.Errorf("volume %q: access point uid = %v, want the non-root %d", volume, uid, nonRootUID)
		}
		if gid := user["gid"].NumberValue(); gid != nonRootUID {
			t.Errorf("volume %q: access point gid = %v, want the non-root %d", volume, gid, nonRootUID)
		}

		root := ap.Inputs["rootDirectory"].ObjectValue()
		if got := root["path"].StringValue(); got != "/"+volume {
			t.Errorf("volume %q: access point root = %q, want /%s", volume, got, volume)
		}
		// Without creationInfo the directory has to exist already, and on
		// a filesystem created empty it never does — the mount then fails
		// at task start rather than at apply.
		info := root["creationInfo"].ObjectValue()
		if uid := info["ownerUid"].NumberValue(); uid != nonRootUID {
			t.Errorf("volume %q: root directory ownerUid = %v, want %d", volume, uid, nonRootUID)
		}
		if gid := info["ownerGid"].NumberValue(); gid != nonRootUID {
			t.Errorf("volume %q: root directory ownerGid = %v, want %d", volume, gid, nonRootUID)
		}
	}
}

// assertFilesystemPolicies covers the resource policy of §2.6: a mount is
// allowed only through a mount target and only over TLS.
//
// The two statements answer two different attackers. The allow's condition
// means a stolen credential cannot reach the filesystem over the EFS API
// from outside the VPC; the deny means a client inside it cannot fall back
// to plaintext NFS. Neither statement grants root.
func assertFilesystemPolicies(t *testing.T, recorded []recordedResource, volumeOf map[string]string) {
	t.Helper()

	policies := resourcesOfType(recorded, fileSystemPolicyToken)
	if len(policies) != len(volumeOf) {
		t.Fatalf("declared %d file system policies, want one per filesystem (%d)",
			len(policies), len(volumeOf))
	}

	for _, p := range policies {
		volume := volumeOf[p.Inputs["fileSystemId"].StringValue()]
		raw := p.Inputs["policy"].StringValue()

		var document struct {
			Version   string `json:"Version"`
			Statement []struct {
				Sid       string                       `json:"Sid"`
				Effect    string                       `json:"Effect"`
				Action    any                          `json:"Action"`
				Condition map[string]map[string]string `json:"Condition"`
			} `json:"Statement"`
		}
		if err := json.Unmarshal([]byte(raw), &document); err != nil {
			t.Fatalf("volume %q: policy is not valid JSON: %v", volume, err)
		}

		var sawMountTargetAllow, sawTLSDeny bool
		for _, st := range document.Statement {
			switch st.Effect {
			case "Allow":
				actions := actionSet(st.Action)
				for _, want := range []string{"elasticfilesystem:ClientMount", "elasticfilesystem:ClientWrite"} {
					if !actions[want] {
						t.Errorf("volume %q: the allow statement does not grant %s", volume, want)
					}
				}
				if st.Condition["Bool"]["elasticfilesystem:AccessedViaMountTarget"] == "true" {
					sawMountTargetAllow = true
				}
			case "Deny":
				if st.Condition["Bool"]["aws:SecureTransport"] == "false" {
					sawTLSDeny = true
				}
			default:
				t.Errorf("volume %q: statement %q has effect %q", volume, st.Sid, st.Effect)
			}
		}

		if !sawMountTargetAllow {
			t.Errorf("volume %q: nothing restricts the allow to AccessedViaMountTarget; "+
				"the filesystem is reachable from outside the VPC with a credential alone", volume)
		}
		if !sawTLSDeny {
			t.Errorf("volume %q: nothing denies aws:SecureTransport=false; "+
				"a client may mount in plaintext", volume)
		}
		// Granted nowhere, checked over the whole document rather than per
		// statement: root access is the one permission that would make the
		// access point's non-root user pointless.
		if strings.Contains(raw, "ClientRootAccess") {
			t.Errorf("volume %q: the file system policy grants ClientRootAccess", volume)
		}
	}
}

// TestDeclareScopeNetworkWithoutVolumesDeclaresNoFilesystem is the other
// half of the contract, and the one that keeps every existing scope
// unchanged: a scope whose resources mount nothing pays for nothing.
func TestDeclareScopeNetworkWithoutVolumesDeclaresNoFilesystem(t *testing.T) {
	cidr := netip.MustParsePrefix("10.42.0.0/20")

	recorded := runProgram(t, func(ctx *pulumi.Context) error {
		return declareScopeNetwork(ctx, testScope(), cidr, provider.ScopeContents{})
	})

	for _, token := range []string{
		fileSystemToken, fileSystemPolicyToken, mountTargetToken,
		accessPointToken, efsBackupPolicyToken, securityGroupToken,
	} {
		if n := len(resourcesOfType(recorded, token)); n != 0 {
			t.Errorf("declared %d %q with no volumes in the scope, want none", n, token)
		}
	}
}

// TestFileSystemTokenIsDerivable is what lets two stacks agree on a
// filesystem without talking to each other: the network stack sets the
// creation token and the mounting stack recomputes it from the same scope
// and volume name (RFC 020 §2.5).
func TestFileSystemTokenIsDerivable(t *testing.T) {
	tests := []struct {
		name   string
		scope  provider.NetworkScope
		volume string
		want   string
	}{
		{
			name:   "environment and region",
			scope:  provider.NetworkScope{Environment: "dev", Region: "eu-central-1"},
			volume: "uploads",
			want:   "cloudsdd::dev::eu-central-1::uploads",
		},
		{
			// Two accounts never share a network, and so never share a
			// filesystem either (RFC 016 §2.1).
			name:   "account included",
			scope:  provider.NetworkScope{Account: "prod", Environment: "live", Region: "eu-west-1"},
			volume: "uploads",
			want:   "cloudsdd::prod::live::eu-west-1::uploads",
		},
		{
			name:   "unscoped",
			scope:  provider.NetworkScope{},
			volume: "uploads",
			want:   "cloudsdd::default::uploads",
		},
		{
			// The volume name is carried verbatim rather than folded to
			// lowercase: a volume name is case-sensitive in the
			// Specification, so "Cache" and "cache" are two filesystems and
			// must stay two creation tokens.
			name:   "case is preserved",
			scope:  provider.NetworkScope{Environment: "dev", Region: "eu-central-1"},
			volume: "Cache",
			want:   "cloudsdd::dev::eu-central-1::Cache",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fileSystemTokenFor(tt.scope, tt.volume)
			if got != tt.want {
				t.Errorf("fileSystemTokenFor() = %q, want %q", got, tt.want)
			}
			if len([]rune(got)) > maxCreationToken {
				t.Errorf("token %q is %d characters, EFS accepts %d",
					got, len([]rune(got)), maxCreationToken)
			}
		})
	}
}

// TestFileSystemTokenFitsTheLimit covers the truncation path, which is the
// one place the derivation can go wrong invisibly: EFS accepts 64
// characters, a scope label plus a volume name can exceed that, and two
// tokens that collide after the cut would be two volumes sharing one
// filesystem.
func TestFileSystemTokenFitsTheLimit(t *testing.T) {
	long := provider.NetworkScope{
		Account:     strings.Repeat("a", 32),
		Environment: strings.Repeat("e", 32),
		Region:      strings.Repeat("r", 32),
	}

	// Same tail, and it is the tail that gets cut. Only the hash of the
	// untruncated form can still tell these apart.
	first := fileSystemTokenFor(long, "a-volume-with-a-long-name-first")
	second := fileSystemTokenFor(long, "a-volume-with-a-long-name-second")

	for _, got := range []string{first, second} {
		if n := len([]rune(got)); n > maxCreationToken {
			t.Errorf("token %q is %d characters, EFS accepts %d", got, n, maxCreationToken)
		}
		if !strings.HasPrefix(got, "cloudsdd") {
			t.Errorf("token %q does not identify itself as CloudSDD's", got)
		}
	}
	if first == second {
		t.Errorf("two volumes truncated onto the same token %q; they would share one filesystem", first)
	}

	// Derivation, not generation: the mounting stack computes the same
	// string from the same inputs, on another machine and at another time.
	if again := fileSystemTokenFor(long, "a-volume-with-a-long-name-first"); again != first {
		t.Errorf("fileSystemTokenFor() = %q on the second call, %q on the first", again, first)
	}
}

// filesystemFor picks the filesystem declared under a creation token.
func filesystemFor(filesystems []recordedResource, token string) (recordedResource, bool) {
	for _, fs := range filesystems {
		if fs.Inputs["creationToken"].StringValue() == token {
			return fs, true
		}
	}
	return recordedResource{}, false
}

func creationTokens(filesystems []recordedResource) []string {
	out := make([]string, 0, len(filesystems))
	for _, fs := range filesystems {
		out = append(out, fs.Inputs["creationToken"].StringValue())
	}
	return out
}

// subnetIDsByTier is the set of subnet IDs the network declared in one
// tier, in the mock monitor's terms.
func subnetIDsByTier(recorded []recordedResource, tier string) map[string]bool {
	ids := map[string]bool{}
	for _, s := range resourcesOfType(recorded, subnetToken) {
		if s.Inputs["tags"].ObjectValue()[tagSubnetTier].StringValue() == tier {
			ids[mockID(s)] = true
		}
	}
	return ids
}

// actionSet reads an IAM Action element, which is a string or an array of
// them depending on how many actions a statement grants.
func actionSet(action any) map[string]bool {
	actions := map[string]bool{}
	switch v := action.(type) {
	case string:
		actions[v] = true
	case []any:
		for _, a := range v {
			if s, ok := a.(string); ok {
				actions[s] = true
			}
		}
	}
	return actions
}
