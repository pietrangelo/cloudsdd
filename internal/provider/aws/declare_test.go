// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package aws

import (
	"fmt"
	"sync"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

type recordedResource struct {
	Type   string
	Name   string
	Inputs resource.PropertyMap
}

// recorder collects declared resources. Pulumi registers resources
// concurrently, so access to the slice must be synchronized.
type recorder struct {
	mu        sync.Mutex
	resources []recordedResource
}

func (r *recorder) add(res recordedResource) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resources = append(r.resources, res)
}

func (r *recorder) snapshot() []recordedResource {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedResource(nil), r.resources...)
}

// mockMonitor captures declared resources without contacting AWS.
type mockMonitor struct {
	rec *recorder

	// efs is what the scope's network stack left behind, as the two
	// lookups of RFC 020 §2.8 see it. The zero value is the ordinary
	// scope, which is what every test that is not about a broken one
	// wants.
	efs efsScope
}

// efsScope lets a test describe a scope whose filesystems are not where
// the mounting stack expects them.
//
// Both cases it can describe are broken invariants rather than races: the
// Engine provisions a scope's network stack before any resource in it is
// applied, so a filesystem that is absent here was removed out of band.
// The provider must refuse, because the alternative — declaring a mount
// against something that is not there — fails at task start, long after
// the user approved a plan that looked fine.
type efsScope struct {
	// missingToken is a creation token no filesystem answers to. The real
	// getFileSystem invoke fails when nothing matches, and so does this.
	missingToken string

	// withoutAccessPoints strips every filesystem of the access point the
	// network stack gave it (RFC 020 §2.6). The filesystem is then present
	// and the identity a task would reach it through is not.
	withoutAccessPoints bool
}

func (m mockMonitor) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	m.rec.add(recordedResource{
		Type:   args.TypeToken,
		Name:   args.Name,
		Inputs: args.Inputs,
	})

	outputs := args.Inputs.Copy()
	// Outputs the real providers compute and that dependent resources read
	// back. Without them the power schedules of RFC 012 §4.1 would see an
	// unknown instance ARN and their policies could not be asserted on.
	switch args.TypeToken {
	case "random:index/randomPassword:RandomPassword":
		outputs["result"] = resource.NewStringProperty("generated-password")
	case rdsInstanceToken:
		outputs["arn"] = resource.NewStringProperty(testInstanceARN)
		outputs["identifier"] = resource.NewStringProperty(args.Name)
	case iamRoleToken:
		outputs["arn"] = resource.NewStringProperty("arn:aws:iam::" + testAccountID + ":role/" + args.Name)
		// An inline policy names the role it is attached to by name, so
		// without this the two roles of RFC 017 §2.6 are indistinguishable
		// in the recorded inputs — and a permission granted to the
		// execution role instead of the task role would read the same.
		outputs["name"] = resource.NewStringProperty(args.Name)
	case ecrRepositoryToken:
		// The build role's policy is built from the ARN and the buildspec
		// from the URL, so both have to be knowable here or the
		// assertions in pipeline_test.go would run against unknowns
		// (RFC 018 §2.8).
		name := args.Inputs["name"].StringValue()
		outputs["arn"] = resource.NewStringProperty(
			"arn:aws:ecr:eu-central-1:" + testAccountID + ":repository/" + name)
		outputs["repositoryUrl"] = resource.NewStringProperty(
			testAccountID + ".dkr.ecr.eu-central-1.amazonaws.com/" + name)
	case ec2InstanceToken:
		outputs["arn"] = resource.NewStringProperty(testEC2ARN)
	case loadBalancerToken:
		// The alias record targets these, and a Route 53 alias with an
		// unknown target is a record that resolves nowhere.
		outputs["dnsName"] = resource.NewStringProperty(args.Name + ".eu-central-1.elb.amazonaws.com")
		outputs["zoneId"] = resource.NewStringProperty("Z215JYRZR1TBD5")
	case certificateToken:
		outputs["arn"] = resource.NewStringProperty(
			"arn:aws:acm:eu-central-1:" + testAccountID + ":certificate/" + args.Name)
		// The DNS challenge ACM computes. The provider indexes the first
		// entry — safe because CloudSDD requests one domain and no subject
		// alternative names — so an empty array here would panic rather
		// than fail an assertion (RFC 017 §2.3.1).
		outputs["domainValidationOptions"] = resource.NewArrayProperty([]resource.PropertyValue{
			resource.NewObjectProperty(resource.PropertyMap{
				"domainName":          args.Inputs["domainName"],
				"resourceRecordName":  resource.NewStringProperty("_acme-challenge.api.acme.example."),
				"resourceRecordType":  resource.NewStringProperty("CNAME"),
				"resourceRecordValue": resource.NewStringProperty("validation.acm-validations.aws."),
			}),
		})
	case certValidationToken:
		outputs["certificateArn"] = args.Inputs["certificateArn"]
	case route53RecordToken:
		outputs["fqdn"] = args.Inputs["name"]
	case ecsClusterToken:
		outputs["arn"] = resource.NewStringProperty(
			"arn:aws:ecs:eu-central-1:" + testAccountID + ":cluster/" + args.Name)
	case ecsServiceToken:
		// An ECS service exposes no separate Arn output: its *ID* is the
		// ARN, which is what the schedule's trust and permission policies
		// are built from (RFC 017 §2.5). Returning a bare name here would
		// pass the test and misrepresent the resource.
		outputs["name"] = resource.NewStringProperty(args.Name)
		return "arn:aws:ecs:eu-central-1:" + testAccountID + ":service/" + args.Name, outputs, nil
	}
	return args.Name + "-id", outputs, nil
}

// Call answers the provider's invokes. The AMI lookup of RFC 013 §2.1 is
// the only one; it returns a fixed ID and echoes the filters back so a
// test can assert what was actually asked for.
func (m mockMonitor) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	// The image repository a container_service fed by a pipeline resolves
	// its registry host from (RFC 018 §2.4.1).
	if args.Token == getEcrRepositoryToken {
		m.rec.add(recordedResource{Type: args.Token, Name: getEcrRepositoryToken, Inputs: args.Args})
		name := args.Args["name"].StringValue()
		return resource.PropertyMap{
			"name": resource.NewStringProperty(name),
			"arn": resource.NewStringProperty(
				"arn:aws:ecr:eu-central-1:" + testAccountID + ":repository/" + name),
			"repositoryUrl": resource.NewStringProperty(
				testAccountID + ".dkr.ecr.eu-central-1.amazonaws.com/" + name),
		}, nil
	}
	if args.Token == getAmiToken {
		m.rec.add(recordedResource{Type: args.Token, Name: getAmiToken, Inputs: args.Args})
		return resource.PropertyMap{
			"id":   resource.NewStringProperty(testAMI),
			"name": resource.NewStringProperty("resolved-image"),
		}, nil
	}
	// The scope-network discovery a resource program makes (RFC 016
	// §2.2): find the VPC by tag, then its private subnets.
	if args.Token == getVpcToken {
		m.rec.add(recordedResource{Type: args.Token, Name: getVpcToken, Inputs: args.Args})
		return resource.PropertyMap{
			"id":        resource.NewStringProperty("vpc-0123456789"),
			"cidrBlock": resource.NewStringProperty("10.42.0.0/20"),
		}, nil
	}
	if args.Token == getSubnetsToken {
		m.rec.add(recordedResource{Type: args.Token, Name: getSubnetsToken, Inputs: args.Args})
		return resource.PropertyMap{
			"ids": resource.NewArrayProperty([]resource.PropertyValue{
				resource.NewStringProperty("subnet-aaa"),
				resource.NewStringProperty("subnet-bbb"),
			}),
		}, nil
	}
	// The filesystem a mounting resource stack finds rather than creates
	// (RFC 020 §2.8), by the creation token both halves derive from the
	// same scope and volume name.
	if args.Token == getFileSystemToken {
		m.rec.add(recordedResource{Type: args.Token, Name: getFileSystemToken, Inputs: args.Args})
		token := args.Args["creationToken"].StringValue()
		if token == m.efs.missingToken {
			return nil, fmt.Errorf("no EFS file system with creation token %q", token)
		}
		id := testFileSystemID(token)
		return resource.PropertyMap{
			"id":            resource.NewStringProperty(id),
			"fileSystemId":  resource.NewStringProperty(id),
			"creationToken": resource.NewStringProperty(token),
			"arn":           resource.NewStringProperty(testFileSystemARN(id)),
		}, nil
	}
	// The access point that filesystem carries, which is the identity the
	// task mounts as (RFC 020 §2.6). The network stack declares exactly
	// one per filesystem, so the set is a set of one.
	if args.Token == getAccessPointsToken {
		m.rec.add(recordedResource{Type: args.Token, Name: getAccessPointsToken, Inputs: args.Args})
		fileSystemID := args.Args["fileSystemId"].StringValue()
		ids := []resource.PropertyValue{resource.NewStringProperty(testAccessPointID(fileSystemID))}
		if m.efs.withoutAccessPoints {
			ids = nil
		}
		return resource.PropertyMap{
			"id":           resource.NewStringProperty(fileSystemID),
			"fileSystemId": resource.NewStringProperty(fileSystemID),
			"ids":          resource.NewArrayProperty(ids),
		}, nil
	}
	// The hosted zone a public container service's certificate is
	// validated in (RFC 017 §2.3.1).
	if args.Token == getZoneToken {
		m.rec.add(recordedResource{Type: args.Token, Name: getZoneToken, Inputs: args.Args})
		return resource.PropertyMap{
			"zoneId": resource.NewStringProperty("Z0123456789ABCDEFGHIJ"),
			"name":   args.Args["name"],
		}, nil
	}
	// The availability zone lookup the network program makes (RFC 016).
	// Three zones, so a test can tell "took the first two" apart from
	// "took all of them".
	if args.Token == getAvailabilityZonesToken {
		m.rec.add(recordedResource{Type: args.Token, Name: getAvailabilityZonesToken, Inputs: args.Args})
		return resource.PropertyMap{
			"names": resource.NewArrayProperty([]resource.PropertyValue{
				resource.NewStringProperty("eu-central-1a"),
				resource.NewStringProperty("eu-central-1b"),
				resource.NewStringProperty("eu-central-1c"),
			}),
		}, nil
	}
	return resource.PropertyMap{}, nil
}

func runProgram(t *testing.T, fn func(ctx *pulumi.Context) error) []recordedResource {
	t.Helper()

	rec := &recorder{}
	if err := pulumi.RunErr(fn, pulumi.WithMocks("cloudsdd-aws", "test", mockMonitor{rec: rec})); err != nil {
		t.Fatalf("pulumi program failed: %v", err)
	}
	return rec.snapshot()
}

func findResource(t *testing.T, recorded []recordedResource, typeToken string) recordedResource {
	t.Helper()
	for _, r := range recorded {
		if r.Type == typeToken {
			return r
		}
	}
	t.Fatalf("no %q resource was declared; got %v", typeToken, typeTokens(recorded))
	return recordedResource{}
}

func hasResource(recorded []recordedResource, typeToken string) bool {
	for _, r := range recorded {
		if r.Type == typeToken {
			return true
		}
	}
	return false
}

func typeTokens(recorded []recordedResource) []string {
	out := make([]string, 0, len(recorded))
	for _, r := range recorded {
		out = append(out, r.Type)
	}
	return out
}

const rdsInstanceToken = "aws:rds/instance:Instance"

// TestDeclareRelationalDatabaseSecureDefaults pins the RFC 011 §2.5
// reversal end-to-end: SkipFinalSnapshot shipped hardcoded true "for
// simplified teardown during dev", so a destroy discarded the database
// with no recovery point at all.
func TestDeclareRelationalDatabaseSecureDefaults(t *testing.T) {
	props := relationalDatabaseProperties{Engine: "postgres", Version: "15"}

	recorded := runProgram(t, func(ctx *pulumi.Context) error {
		_, err := declareRelationalDatabase(ctx, "app-db", props, testNetwork())
		return err
	})
	instance := findResource(t, recorded, rdsInstanceToken)

	if !instance.Inputs["deletionProtection"].BoolValue() {
		t.Error("deletionProtection = false, want true by default (RFC 011 §2.5)")
	}
	if instance.Inputs["skipFinalSnapshot"].BoolValue() {
		t.Error("skipFinalSnapshot = true, want false by default (RFC 011 §2.5)")
	}
	// RDS requires an identifier whenever a final snapshot is taken,
	// which is now the default — omitting it would fail at apply time.
	id, ok := instance.Inputs["finalSnapshotIdentifier"]
	if !ok {
		t.Fatal("finalSnapshotIdentifier is unset while a final snapshot is required")
	}
	if id.StringValue() != "app-db-final-snapshot" {
		t.Errorf("finalSnapshotIdentifier = %q, want it derived from the resource ID", id.StringValue())
	}

	if instance.Inputs["publiclyAccessible"].BoolValue() {
		t.Error("publiclyAccessible = true, want false")
	}
	if !instance.Inputs["storageEncrypted"].BoolValue() {
		t.Error("storageEncrypted = false, want true")
	}
	if !instance.Inputs["iamDatabaseAuthenticationEnabled"].BoolValue() {
		t.Error("iamDatabaseAuthenticationEnabled = false, want true")
	}
	if got := instance.Inputs["backupRetentionPeriod"].NumberValue(); got != 7 {
		t.Errorf("backupRetentionPeriod = %v, want 7", got)
	}
}

func TestDeclareRelationalDatabaseExplicitDisposability(t *testing.T) {
	yes, no := true, false
	props := relationalDatabaseProperties{
		Engine:             "postgres",
		Version:            "15",
		DeletionProtection: &no,
		SkipFinalSnapshot:  &yes,
	}

	recorded := runProgram(t, func(ctx *pulumi.Context) error {
		_, err := declareRelationalDatabase(ctx, "app-db", props, testNetwork())
		return err
	})
	instance := findResource(t, recorded, rdsInstanceToken)

	if instance.Inputs["deletionProtection"].BoolValue() {
		t.Error("deletionProtection = true, want the explicitly requested false")
	}
	if !instance.Inputs["skipFinalSnapshot"].BoolValue() {
		t.Error("skipFinalSnapshot = false, want the explicitly requested true")
	}
	// With no final snapshot there is nothing to name.
	if _, ok := instance.Inputs["finalSnapshotIdentifier"]; ok {
		t.Error("finalSnapshotIdentifier was set even though the final snapshot is skipped")
	}
}

func TestDeclareRelationalDatabaseHonoursEngineAndHA(t *testing.T) {
	tests := []struct {
		name         string
		props        relationalDatabaseProperties
		wantEngine   string
		wantMultiAz  bool
		wantInstance string
	}{
		{
			name:         "postgres single-az",
			props:        relationalDatabaseProperties{Engine: "postgres", Version: "15"},
			wantEngine:   "postgres",
			wantInstance: "db.t3.micro",
		},
		{
			name:         "mysql is not substituted",
			props:        relationalDatabaseProperties{Engine: "mysql", Version: "8.0"},
			wantEngine:   "mysql",
			wantInstance: "db.t3.micro",
		},
		{
			name:         "high availability is multi-az",
			props:        relationalDatabaseProperties{Engine: "postgres", Version: "15", HighAvailability: true},
			wantEngine:   "postgres",
			wantMultiAz:  true,
			wantInstance: "db.t3.small",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorded := runProgram(t, func(ctx *pulumi.Context) error {
				_, err := declareRelationalDatabase(ctx, "app-db", tt.props, testNetwork())
				return err
			})
			instance := findResource(t, recorded, rdsInstanceToken)

			if got := instance.Inputs["engine"].StringValue(); got != tt.wantEngine {
				t.Errorf("engine = %q, want %q", got, tt.wantEngine)
			}
			if got := instance.Inputs["multiAz"].BoolValue(); got != tt.wantMultiAz {
				t.Errorf("multiAz = %v, want %v", got, tt.wantMultiAz)
			}
			if got := instance.Inputs["instanceClass"].StringValue(); got != tt.wantInstance {
				t.Errorf("instanceClass = %q, want %q", got, tt.wantInstance)
			}
		})
	}
}

func TestDeclareRelationalDatabaseGeneratesAPassword(t *testing.T) {
	props := relationalDatabaseProperties{Engine: "postgres", Version: "15"}

	recorded := runProgram(t, func(ctx *pulumi.Context) error {
		_, err := declareRelationalDatabase(ctx, "app-db", props, testNetwork())
		return err
	})

	if !hasResource(recorded, "random:index/randomPassword:RandomPassword") {
		t.Fatal("no RandomPassword was declared; the master password must be generated, not fixed")
	}
	pwd := findResource(t, recorded, "random:index/randomPassword:RandomPassword")
	if got := pwd.Inputs["length"].NumberValue(); got < 32 {
		t.Errorf("password length = %v, want at least 32", got)
	}

	instance := findResource(t, recorded, rdsInstanceToken)
	if got := instance.Inputs["username"].StringValue(); got != "masteruser" {
		t.Errorf("username = %q, want masteruser", got)
	}
}

// TestDeclareS3BucketSecureDefaults pins the pre-existing S3 posture so
// the shared-decoder refactor could not quietly change it.
func TestDeclareS3BucketSecureDefaults(t *testing.T) {
	props := S3Properties{BucketName: "my-bucket"}

	recorded := runProgram(t, func(ctx *pulumi.Context) error {
		return declareS3Bucket(ctx, "my-bucket", props)
	})

	if !hasResource(recorded, "aws:s3/bucketServerSideEncryptionConfigurationV2:BucketServerSideEncryptionConfigurationV2") {
		t.Error("no server-side encryption configuration was declared; encryption must be on by default")
	}
	if !hasResource(recorded, "aws:s3/bucketPublicAccessBlock:BucketPublicAccessBlock") {
		t.Error("no public access block was declared; it must be on by default")
	}

	block := findResource(t, recorded, "aws:s3/bucketPublicAccessBlock:BucketPublicAccessBlock")
	for _, field := range []string{"blockPublicAcls", "blockPublicPolicy", "ignorePublicAcls", "restrictPublicBuckets"} {
		if !block.Inputs[resource.PropertyKey(field)].BoolValue() {
			t.Errorf("%s = false, want true", field)
		}
	}

	versioning := findResource(t, recorded, "aws:s3/bucketVersioningV2:BucketVersioningV2")
	status := versioning.Inputs["versioningConfiguration"].ObjectValue()["status"].StringValue()
	if status != "Disabled" {
		t.Errorf("versioning status = %q, want Disabled by default", status)
	}
}

func TestDeclareS3BucketHonoursExplicitOverrides(t *testing.T) {
	yes, no := true, false
	props := S3Properties{
		BucketName:        "my-bucket",
		Versioning:        &yes,
		Encryption:        &no,
		BlockPublicAccess: &no,
	}

	recorded := runProgram(t, func(ctx *pulumi.Context) error {
		return declareS3Bucket(ctx, "my-bucket", props)
	})

	if hasResource(recorded, "aws:s3/bucketServerSideEncryptionConfigurationV2:BucketServerSideEncryptionConfigurationV2") {
		t.Error("encryption configuration was declared despite encryption:false")
	}
	if hasResource(recorded, "aws:s3/bucketPublicAccessBlock:BucketPublicAccessBlock") {
		t.Error("public access block was declared despite block_public_access:false")
	}

	versioning := findResource(t, recorded, "aws:s3/bucketVersioningV2:BucketVersioningV2")
	status := versioning.Inputs["versioningConfiguration"].ObjectValue()["status"].StringValue()
	if status != "Enabled" {
		t.Errorf("versioning status = %q, want Enabled", status)
	}
}

// testNetwork is the scope network a declaration test places its
// resources in. It stands in for what lookupScopeNetwork returns after
// the Engine has provisioned the scope (RFC 016 §2.2).
func testNetwork() scopeNetwork {
	return scopeNetwork{
		vpcID:     "vpc-test",
		cidr:      "10.42.0.0/20",
		subnetIDs: []string{"subnet-a", "subnet-b"},
		// The public tier, which holds the NAT gateway and — since RFC 017
		// §2.3.1 — an internet-facing load balancer. Two, because an ALB
		// refuses to exist in a single availability zone.
		publicSubnetIDs: []string{"subnet-pub-a", "subnet-pub-b"},
		dbSubnetName:    "cloudsdd-dev-eu-central-1",
	}
}
