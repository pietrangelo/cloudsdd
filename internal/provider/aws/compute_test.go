// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package aws

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/provider/compute"
	"cloudsdd/internal/schedule"
	"cloudsdd/internal/spec"
)

const (
	getAmiToken        = "aws:ec2/getAmi:getAmi"
	ec2InstanceToken   = "aws:ec2/instance:Instance"
	securityGroupToken = "aws:ec2/securityGroup:SecurityGroup"
	instanceProfileTok = "aws:iam/instanceProfile:InstanceProfile"
	rolePolicyAttachTk = "aws:iam/rolePolicyAttachment:RolePolicyAttachment"

	testAMI    = "ami-0123456789abcdef0"
	testEC2ARN = "arn:aws:ec2:eu-central-1:" + testAccountID + ":instance/i-0123456789abcdef0"
)

func declareVM(t *testing.T, p compute.Properties, zone string) []recordedResource {
	t.Helper()
	return runProgram(t, func(ctx *pulumi.Context) error {
		_, err := declareComputeInstance(ctx, "build-agent", p, zone, nil)
		return err
	})
}

func defaultVMProperties() compute.Properties {
	return compute.Properties{Size: compute.SizeSmall, OS: compute.OSUbuntu2204}
}

func TestDeclareComputeInstanceSecureDefaults(t *testing.T) {
	recorded := declareVM(t, defaultVMProperties(), "")
	instance := findResource(t, recorded, ec2InstanceToken)
	m := instance.Inputs.Mappable()

	// IMDSv2. The classic EC2 compromise is an application SSRF that
	// reaches the metadata service and leaves with the role's
	// credentials; this is the control that breaks the chain.
	metadata, _ := m["metadataOptions"].(map[string]any)
	if metadata == nil {
		t.Fatal("no metadataOptions; the instance would accept IMDSv1 requests")
	}
	if got := metadata["httpTokens"]; got != "required" {
		t.Errorf("httpTokens = %v, want \"required\" (IMDSv2 only)", got)
	}
	if got := metadata["httpPutResponseHopLimit"]; got != 1.0 {
		t.Errorf("httpPutResponseHopLimit = %v, want 1, so a container on the host cannot reach the metadata service", got)
	}

	root, _ := m["rootBlockDevice"].(map[string]any)
	if root == nil || root["encrypted"] != true {
		t.Errorf("rootBlockDevice = %v, want an encrypted volume", m["rootBlockDevice"])
	}
	if got := root["volumeSize"]; got != 20.0 {
		t.Errorf("volumeSize = %v, want the 20GB default", got)
	}

	if m["associatePublicIpAddress"] != false {
		t.Errorf("associatePublicIpAddress = %v, want false: private by default", m["associatePublicIpAddress"])
	}
	// No key pair: access is through Session Manager, so no long-lived
	// key exists to be leaked or rotated.
	if _, ok := m["keyName"]; ok {
		t.Errorf("a key pair was attached (%v); access must go through Session Manager", m["keyName"])
	}
}

func TestDeclareComputeInstanceHasNoInboundAccess(t *testing.T) {
	recorded := declareVM(t, defaultVMProperties(), "")
	sg := findResource(t, recorded, securityGroupToken).Inputs.Mappable()

	// Not "no rule allowing 22" — no ingress rules at all.
	if ingress, ok := sg["ingress"]; ok {
		if rules, _ := ingress.([]any); len(rules) > 0 {
			t.Errorf("security group has %d ingress rules, want none", len(rules))
		}
	}

	// Egress stays open so the SSM agent can dial out and the machine can
	// patch itself; closing it needs VPC endpoints this schema cannot yet
	// express.
	egress, _ := sg["egress"].([]any)
	if len(egress) != 1 {
		t.Fatalf("egress rules = %v, want exactly one allow-all", sg["egress"])
	}
}

func TestDeclareComputeInstanceGrantsOnlySessionManager(t *testing.T) {
	recorded := declareVM(t, defaultVMProperties(), "")

	attachment := findResource(t, recorded, rolePolicyAttachTk)
	if got := attachment.Inputs["policyArn"].StringValue(); got != ssmManagedInstancePolicy {
		t.Errorf("attached policy = %q, want only %q", got, ssmManagedInstancePolicy)
	}
	// A VM runs arbitrary code with an attached identity, so exactly one
	// managed policy and no inline policy of its own.
	if n := len(resourcesOfType(recorded, rolePolicyAttachTk)); n != 1 {
		t.Errorf("role has %d policy attachments, want 1", n)
	}
	if n := len(resourcesOfType(recorded, iamRolePolicyToken)); n != 0 {
		t.Errorf("role has %d inline policies, want none", n)
	}

	role := findResource(t, recorded, iamRoleToken)
	var doc iamPolicyDocument
	if err := json.Unmarshal([]byte(role.Inputs["assumeRolePolicy"].StringValue()), &doc); err != nil {
		t.Fatalf("trust policy is not valid JSON: %v", err)
	}
	if doc.Statement[0].Principal == nil || doc.Statement[0].Principal.Service != "ec2.amazonaws.com" {
		t.Errorf("trust principal = %+v, want the EC2 service", doc.Statement[0].Principal)
	}

	if !hasResource(recorded, instanceProfileTok) {
		t.Error("no instance profile was declared; Session Manager could not reach the instance")
	}
}

func TestDeclareComputeInstanceImageLookupIsOwnerFiltered(t *testing.T) {
	// Filtering on a name pattern alone would let any account publishing a
	// public AMI with a matching name be selected (RFC 013 §4).
	tests := []struct {
		os        compute.OS
		wantOwner string
		wantName  string
	}{
		{os: compute.OSUbuntu2204, wantOwner: canonicalOwnerID, wantName: "jammy"},
		{os: compute.OSUbuntu2404, wantOwner: canonicalOwnerID, wantName: "noble"},
		{os: compute.OSDebian12, wantOwner: debianOwnerID, wantName: "debian-12"},
	}

	for _, tt := range tests {
		t.Run(string(tt.os), func(t *testing.T) {
			p := defaultVMProperties()
			p.OS = tt.os
			recorded := declareVM(t, p, "")

			lookup := findResource(t, recorded, getAmiToken).Inputs.Mappable()
			owners, _ := lookup["owners"].([]any)
			if len(owners) != 1 || owners[0] != tt.wantOwner {
				t.Fatalf("owners = %v, want exactly [%s]", lookup["owners"], tt.wantOwner)
			}

			filters, _ := lookup["filters"].([]any)
			var namePattern string
			for _, f := range filters {
				fm, _ := f.(map[string]any)
				if fm["name"] == "name" {
					values, _ := fm["values"].([]any)
					if len(values) > 0 {
						namePattern, _ = values[0].(string)
					}
				}
			}
			if !strings.Contains(namePattern, tt.wantName) {
				t.Errorf("name filter = %q, want it to select %s", namePattern, tt.wantName)
			}

			instance := findResource(t, recorded, ec2InstanceToken)
			if got := instance.Inputs["ami"].StringValue(); got != testAMI {
				t.Errorf("ami = %q, want the resolved lookup result %q", got, testAMI)
			}
		})
	}
}

func TestDeclareComputeInstanceHonoursSizeAndZone(t *testing.T) {
	tests := []struct {
		size     compute.Size
		wantType string
	}{
		{size: compute.SizeSmall, wantType: "t3.small"},
		{size: compute.SizeMedium, wantType: "t3.medium"},
		{size: compute.SizeLarge, wantType: "t3.large"},
	}

	for _, tt := range tests {
		t.Run(string(tt.size), func(t *testing.T) {
			p := defaultVMProperties()
			p.Size = tt.size
			recorded := declareVM(t, p, "eu-central-1b")
			instance := findResource(t, recorded, ec2InstanceToken)

			if got := instance.Inputs["instanceType"].StringValue(); got != tt.wantType {
				t.Errorf("instanceType = %q, want %q", got, tt.wantType)
			}
			if got := instance.Inputs["availabilityZone"].StringValue(); got != "eu-central-1b" {
				t.Errorf("availabilityZone = %q, want the requested zone", got)
			}
		})
	}
}

func TestDeclareComputeInstanceOmitsZoneWhenUnset(t *testing.T) {
	recorded := declareVM(t, defaultVMProperties(), "")
	instance := findResource(t, recorded, ec2InstanceToken)

	if _, ok := instance.Inputs["availabilityZone"]; ok {
		t.Error("an availability zone was pinned though none was requested; the cloud should choose")
	}
}

func TestDeclareComputeInstanceExplicitPublicIP(t *testing.T) {
	yes := true
	p := defaultVMProperties()
	p.PublicIP = &yes
	p.DiskSizeGB = 100

	recorded := declareVM(t, p, "")
	instance := findResource(t, recorded, ec2InstanceToken).Inputs.Mappable()

	if instance["associatePublicIpAddress"] != true {
		t.Error("associatePublicIpAddress = false, want the explicitly requested true")
	}
	root, _ := instance["rootBlockDevice"].(map[string]any)
	if got := root["volumeSize"]; got != 100.0 {
		t.Errorf("volumeSize = %v, want the requested 100", got)
	}
	// A public address must not imply an open door.
	sg := findResource(t, recorded, securityGroupToken).Inputs.Mappable()
	if ingress, ok := sg["ingress"]; ok {
		if rules, _ := ingress.([]any); len(rules) > 0 {
			t.Errorf("a public instance gained %d ingress rules; it must stay closed", len(rules))
		}
	}
}

func TestComputeScheduleTargetsTheInstance(t *testing.T) {
	rules, err := schedule.Compile(workWeekSchedule())
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	recorded := runProgram(t, func(ctx *pulumi.Context) error {
		instance, err := declareComputeInstance(ctx, "build-agent", defaultVMProperties(), "", nil)
		if err != nil {
			return err
		}
		return declareComputeSchedule(ctx, "build-agent", instance, rules)
	})

	schedules := resourcesOfType(recorded, scheduleToken)
	if len(schedules) != 2 {
		t.Fatalf("declared %d schedules, want 2", len(schedules))
	}
	for _, s := range schedules {
		m := s.Inputs.Mappable()
		name, _ := m["name"].(string)
		target, _ := m["target"].(map[string]any)

		wantAPI := stopInstancesTarget
		if strings.Contains(name, "start") {
			wantAPI = startInstancesTarget
		}
		if got := target["arn"]; got != wantAPI {
			t.Errorf("%s target = %v, want %s", name, got, wantAPI)
		}

		// ec2:StartInstances takes a list even for one machine.
		var payload struct {
			InstanceIDs []string `json:"InstanceIds"`
		}
		input, _ := target["input"].(string)
		if err := json.Unmarshal([]byte(input), &payload); err != nil {
			t.Fatalf("%s input is not JSON: %q", name, input)
		}
		if len(payload.InstanceIDs) != 1 || payload.InstanceIDs[0] == "" {
			t.Errorf("%s targets %v, want exactly the declared instance", name, payload.InstanceIDs)
		}
	}

	// The execution role may start and stop this instance and nothing else.
	policy := findResource(t, recorded, iamRolePolicyToken)
	var doc iamPolicyDocument
	if err := json.Unmarshal([]byte(policy.Inputs["policy"].StringValue()), &doc); err != nil {
		t.Fatalf("permission policy is not valid JSON: %v", err)
	}
	wantActions := map[string]bool{"ec2:StartInstances": true, "ec2:StopInstances": true}
	if len(doc.Statement[0].Action) != 2 {
		t.Fatalf("actions = %v, want exactly %v", doc.Statement[0].Action, wantActions)
	}
	for _, a := range doc.Statement[0].Action {
		if !wantActions[a] {
			t.Errorf("unexpected action %q", a)
		}
	}
}

func TestDeclareComputeInstanceRefusesUnmappedValues(t *testing.T) {
	// Validate normally catches these first, but the declaration is also
	// reachable from Destroy, which does not validate (the shape that made
	// the Azure provider panic in RFC 011 §1.1B1). It must return an error
	// rather than provision against an empty instance type or image.
	tests := []struct {
		name    string
		props   compute.Properties
		wantErr error
	}{
		{
			name:    "unmapped size",
			props:   compute.Properties{Size: "gigantic", OS: compute.OSUbuntu2204},
			wantErr: ErrUnsupportedSize,
		},
		{
			name:    "unmapped os",
			props:   compute.Properties{Size: compute.SizeSmall, OS: "plan9"},
			wantErr: ErrUnsupportedOS,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var declareErr error
			_ = runProgram(t, func(ctx *pulumi.Context) error {
				_, declareErr = declareComputeInstance(ctx, "build-agent", tt.props, "", nil)
				return nil
			})
			if !errors.Is(declareErr, tt.wantErr) {
				t.Fatalf("declareComputeInstance() error = %v, want %v", declareErr, tt.wantErr)
			}
		})
	}
}

func TestMachineTypeAndImageFilterRejectUnknownValues(t *testing.T) {
	// An unmapped value must be an error, never a fallback: silently
	// substituting a different machine or image is the failure mode an
	// intent-driven system cannot tolerate.
	if _, err := machineType("gigantic"); !errors.Is(err, ErrUnsupportedSize) {
		t.Errorf("machineType() error = %v, want %v", err, ErrUnsupportedSize)
	}
	if _, _, err := imageFilter("plan9"); !errors.Is(err, ErrUnsupportedOS) {
		t.Errorf("imageFilter() error = %v, want %v", err, ErrUnsupportedOS)
	}
}

func TestDecodeComputeInstanceProperties(t *testing.T) {
	tests := []struct {
		name    string
		props   map[string]any
		wantErr string
	}{
		{
			name:  "valid",
			props: map[string]any{"size": "small", "os": "ubuntu-22.04"},
		},
		{
			// A user reaching for a raw AMI is asking for something this
			// schema does not offer; dropping it silently would hand back
			// a different image than they named.
			name:    "raw ami is rejected rather than dropped",
			props:   map[string]any{"size": "small", "os": "ubuntu-22.04", "ami": "ami-123"},
			wantErr: "unknown or malformed property",
		},
		{
			name:    "unmapped size",
			props:   map[string]any{"size": "gigantic", "os": "ubuntu-22.04"},
			wantErr: "property validation failed",
		},
		{
			name:    "unmapped os",
			props:   map[string]any{"size": "small", "os": "amazon-linux-2023"},
			wantErr: "property validation failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeComputeInstanceProperties(tt.props)

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("decode error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("decode error = nil, want one containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("decode error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateComputeInstance(t *testing.T) {
	base := func(mutate func(*spec.Resource)) spec.Resource {
		r := spec.Resource{
			ID: "build-agent", Type: spec.ResourceTypeComputeInstance, Provider: spec.ProviderAWS,
			Scope:      spec.Scope{Region: "eu-central-1"},
			Properties: map[string]any{"size": "small", "os": "ubuntu-22.04"},
		}
		if mutate != nil {
			mutate(&r)
		}
		return r
	}

	tests := []struct {
		name     string
		resource spec.Resource
		policies spec.Policies
		wantErr  error
	}{
		{name: "valid", resource: base(nil)},
		{
			name:     "one zone is accepted",
			resource: base(func(r *spec.Resource) { r.Scope.Zones = []string{"eu-central-1a"} }),
		},
		{
			// A single instance cannot span zones, and taking the first
			// entry would not match what the user wrote.
			name:     "two zones are refused",
			resource: base(func(r *spec.Resource) { r.Scope.Zones = []string{"eu-central-1a", "eu-central-1b"} }),
			wantErr:  compute.ErrMultipleZones,
		},
		{
			name:     "region required",
			resource: base(func(r *spec.Resource) { r.Scope.Region = "" }),
			wantErr:  ErrRegionRequired,
		},
		{
			name:     "region outside the policy",
			resource: base(nil),
			policies: spec.Policies{AllowedRegions: []string{"us-east-1"}},
			wantErr:  ErrRegionNotAllowed,
		},
	}

	p := &AWSProvider{stateDir: t.TempDir(), passphrase: "test"}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := p.Validate(t.Context(), tt.resource, tt.policies)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}
