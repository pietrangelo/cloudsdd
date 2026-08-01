// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package gcp

import (
	"errors"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/provider/compute"
	"cloudsdd/internal/schedule"
	"cloudsdd/internal/spec"
)

const (
	gceInstanceToken   = "gcp:compute/instance:Instance"
	firewallToken      = "gcp:compute/firewall:Firewall"
	resourcePolicyTokn = "gcp:compute/resourcePolicy:ResourcePolicy"
)

func defaultVMProperties() compute.Properties {
	return compute.Properties{Size: compute.SizeSmall, OS: compute.OSUbuntu2204}
}

func declareVM(t *testing.T, p compute.Properties, zone string, rules []schedule.Rule) []recordedResource {
	t.Helper()
	return runProgram(t, func(ctx *pulumi.Context) error {
		_, err := declareComputeInstance(ctx, "build-agent", "europe-west1", zone, testNetworkName, p, rules)
		return err
	})
}

func TestDeclareComputeInstanceSecureDefaults(t *testing.T) {
	recorded := declareVM(t, defaultVMProperties(), "", nil)
	instance := findResource(t, recorded, gceInstanceToken).Inputs.Mappable()

	// Shielded VM: verified boot, vTPM and integrity monitoring, so a
	// compromised boot chain is detectable rather than invisible.
	shielded, _ := instance["shieldedInstanceConfig"].(map[string]any)
	if shielded == nil {
		t.Fatal("no shieldedInstanceConfig; the instance boots unverified")
	}
	for _, field := range []string{"enableSecureBoot", "enableVtpm", "enableIntegrityMonitoring"} {
		if shielded[field] != true {
			t.Errorf("%s = %v, want true", field, shielded[field])
		}
	}

	metadata, _ := instance["metadata"].(map[string]any)
	if metadata["enable-oslogin"] != "TRUE" {
		t.Errorf("enable-oslogin = %v, want TRUE: shells must authenticate against IAM", metadata["enable-oslogin"])
	}
	// Without this, keys sitting in project metadata still grant access
	// and OS Login is decorative.
	if metadata["block-project-ssh-keys"] != "TRUE" {
		t.Errorf("block-project-ssh-keys = %v, want TRUE", metadata["block-project-ssh-keys"])
	}
	if metadata["serial-port-enable"] != "FALSE" {
		t.Errorf("serial-port-enable = %v, want FALSE", metadata["serial-port-enable"])
	}

	// No identity attached: the default compute service account would be
	// a standing credential on a machine running arbitrary code.
	if sa, ok := instance["serviceAccount"]; ok {
		t.Errorf("a service account was attached (%v); want none", sa)
	}

	// No public address, so no access config on the interface.
	nics, _ := instance["networkInterfaces"].([]any)
	if len(nics) != 1 {
		t.Fatalf("networkInterfaces = %v, want exactly one", instance["networkInterfaces"])
	}
	nic, _ := nics[0].(map[string]any)
	if configs, ok := nic["accessConfigs"]; ok {
		if list, _ := configs.([]any); len(list) > 0 {
			t.Errorf("accessConfigs = %v, want none: private by default", configs)
		}
	}

	boot, _ := instance["bootDisk"].(map[string]any)
	params, _ := boot["initializeParams"].(map[string]any)
	if got := params["size"]; got != 20.0 {
		t.Errorf("boot disk size = %v, want the 20GB default", got)
	}
}

func TestDeclareComputeInstanceDeniesAllInbound(t *testing.T) {
	// GCP's default network ships default-allow-ssh, permitting port 22
	// from anywhere. Firewall rules are allow-only by default, so nothing
	// short of a deny rule at priority 0 actually closes the instance.
	recorded := declareVM(t, defaultVMProperties(), "", nil)
	fw := findResource(t, recorded, firewallToken).Inputs.Mappable()

	if got := fw["direction"]; got != "INGRESS" {
		t.Errorf("direction = %v, want INGRESS", got)
	}
	if got := fw["priority"]; got != 0.0 {
		t.Errorf("priority = %v, want 0 so it outranks default-allow-ssh", got)
	}
	denies, _ := fw["denies"].([]any)
	if len(denies) != 1 {
		t.Fatalf("denies = %v, want one all-protocol deny", fw["denies"])
	}
	if deny, _ := denies[0].(map[string]any); deny["protocol"] != "all" {
		t.Errorf("deny protocol = %v, want all", deny["protocol"])
	}
	sources, _ := fw["sourceRanges"].([]any)
	if len(sources) != 1 || sources[0] != "0.0.0.0/0" {
		t.Errorf("sourceRanges = %v, want the whole internet denied", fw["sourceRanges"])
	}

	// The rule must actually select this instance.
	tags, _ := fw["targetTags"].([]any)
	if len(tags) != 1 {
		t.Fatalf("targetTags = %v, want exactly one", fw["targetTags"])
	}
	instance := findResource(t, recorded, gceInstanceToken).Inputs.Mappable()
	instanceTags, _ := instance["tags"].([]any)
	if len(instanceTags) != 1 || instanceTags[0] != tags[0] {
		t.Errorf("instance tags = %v, firewall targets %v; the rule would not apply", instanceTags, tags)
	}
}

func TestDeclareComputeInstanceMapping(t *testing.T) {
	tests := []struct {
		name      string
		props     compute.Properties
		zone      string
		wantType  string
		wantImage string
		wantZone  string
	}{
		{
			name:      "small ubuntu, zone derived from the region",
			props:     compute.Properties{Size: compute.SizeSmall, OS: compute.OSUbuntu2204},
			wantType:  "e2-small",
			wantImage: "ubuntu-os-cloud/ubuntu-2204-lts",
			wantZone:  "europe-west1-a",
		},
		{
			name:      "medium ubuntu 24.04",
			props:     compute.Properties{Size: compute.SizeMedium, OS: compute.OSUbuntu2404},
			wantType:  "e2-medium",
			wantImage: "ubuntu-os-cloud/ubuntu-2404-lts-amd64",
			wantZone:  "europe-west1-a",
		},
		{
			name:      "large debian, pinned zone",
			props:     compute.Properties{Size: compute.SizeLarge, OS: compute.OSDebian12},
			zone:      "europe-west1-c",
			wantType:  "e2-standard-2",
			wantImage: "debian-cloud/debian-12",
			wantZone:  "europe-west1-c",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorded := declareVM(t, tt.props, tt.zone, nil)
			instance := findResource(t, recorded, gceInstanceToken).Inputs.Mappable()

			if got := instance["machineType"]; got != tt.wantType {
				t.Errorf("machineType = %v, want %q", got, tt.wantType)
			}
			if got := instance["zone"]; got != tt.wantZone {
				t.Errorf("zone = %v, want %q", got, tt.wantZone)
			}
			boot, _ := instance["bootDisk"].(map[string]any)
			params, _ := boot["initializeParams"].(map[string]any)
			if got := params["image"]; got != tt.wantImage {
				t.Errorf("image = %v, want %q", got, tt.wantImage)
			}
		})
	}
}

func TestDeclareComputeInstanceExplicitPublicIP(t *testing.T) {
	yes := true
	p := defaultVMProperties()
	p.PublicIP = &yes

	recorded := declareVM(t, p, "", nil)
	instance := findResource(t, recorded, gceInstanceToken).Inputs.Mappable()

	nics, _ := instance["networkInterfaces"].([]any)
	nic, _ := nics[0].(map[string]any)
	configs, _ := nic["accessConfigs"].([]any)
	if len(configs) != 1 {
		t.Errorf("accessConfigs = %v, want one for an explicitly public instance", nic["accessConfigs"])
	}

	// A public address must not imply a reachable machine.
	fw := findResource(t, recorded, firewallToken).Inputs.Mappable()
	if denies, _ := fw["denies"].([]any); len(denies) != 1 {
		t.Error("the deny-ingress rule disappeared for a public instance")
	}
}

func TestDeclareComputeInstanceUsesTheNativeSchedule(t *testing.T) {
	// Compute Engine schedules instances itself: no Cloud Scheduler job,
	// no service account, no custom role — all of which Cloud SQL needs.
	rules, err := schedule.Compile(workWeekSchedule())
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	recorded := declareVM(t, defaultVMProperties(), "", rules)

	policy := findResource(t, recorded, resourcePolicyTokn).Inputs.Mappable()
	sched, _ := policy["instanceSchedulePolicy"].(map[string]any)
	if sched == nil {
		t.Fatal("no instanceSchedulePolicy was declared")
	}
	if got := sched["timeZone"]; got != "Europe/Rome" {
		t.Errorf("timeZone = %v, want Europe/Rome", got)
	}
	start, _ := sched["vmStartSchedule"].(map[string]any)
	stop, _ := sched["vmStopSchedule"].(map[string]any)
	if got := start["schedule"]; got != "0 8 * * 1,2,3,4,5" {
		t.Errorf("vmStartSchedule = %v, want the work-week cron", got)
	}
	if got := stop["schedule"]; got != "0 19 * * 1,2,3,4,5" {
		t.Errorf("vmStopSchedule = %v, want the work-week cron", got)
	}
	if got := policy["region"]; got != "europe-west1" {
		t.Errorf("region = %v, want the resource's region", got)
	}

	// The policy has to be attached, or it schedules nothing.
	instance := findResource(t, recorded, gceInstanceToken).Inputs.Mappable()
	if instance["resourcePolicies"] == nil || instance["resourcePolicies"] == "" {
		t.Error("the schedule policy was not attached to the instance")
	}

	// None of the Cloud SQL machinery should appear.
	for _, token := range []string{schedulerJobToken, serviceAccountToken, customRoleToken} {
		if hasResource(recorded, token) {
			t.Errorf("%s was declared; a VM schedules natively", token)
		}
	}
}

func TestDeclareComputeInstanceUnscheduledHasNoPolicy(t *testing.T) {
	recorded := declareVM(t, defaultVMProperties(), "", nil)
	if hasResource(recorded, resourcePolicyTokn) {
		t.Error("a schedule policy was declared for an unscheduled instance")
	}
}

func TestComputeExceptionWindowsAreRefusedWithTheRightReason(t *testing.T) {
	// The two schedulable types hit this limit for different structural
	// reasons; an error naming the wrong one sends the reader looking in
	// the wrong place.
	sch := workWeekSchedule()
	sch.Exceptions = []schedule.Window{
		{From: "2026-12-24", To: "2027-01-06", Mode: schedule.ModeAlwaysOff},
	}

	tests := []struct {
		name     string
		typ      spec.ResourceType
		props    map[string]any
		wantText string
	}{
		{
			name:     "compute instance",
			typ:      spec.ResourceTypeComputeInstance,
			props:    map[string]any{"size": "small", "os": "ubuntu-22.04"},
			wantText: "one schedule policy",
		},
		{
			name:     "relational database",
			typ:      spec.ResourceTypeRelationalDatabase,
			props:    map[string]any{"engine": "postgres", "version": "15"},
			wantText: "Cloud Scheduler job",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := spec.Resource{ID: "res", Type: tt.typ, Provider: spec.ProviderGCP, Properties: tt.props}

			_, err := resourceSchedule(r, spec.Policies{Schedule: sch})
			if !errors.Is(err, ErrScheduleExceptionsUnsupported) {
				t.Fatalf("resourceSchedule() error = %v, want %v", err, ErrScheduleExceptionsUnsupported)
			}
			if !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("error = %q, want it to explain %q", err, tt.wantText)
			}
		})
	}
}

func TestComputeMappingRejectsUnknownValues(t *testing.T) {
	if _, err := machineType("gigantic"); !errors.Is(err, ErrUnsupportedSize) {
		t.Errorf("machineType() error = %v, want %v", err, ErrUnsupportedSize)
	}
	if _, err := imageFamily("plan9"); !errors.Is(err, ErrUnsupportedOS) {
		t.Errorf("imageFamily() error = %v, want %v", err, ErrUnsupportedOS)
	}

	// Reachable from Destroy, which does not validate first.
	for _, p := range []compute.Properties{
		{Size: "gigantic", OS: compute.OSUbuntu2204},
		{Size: compute.SizeSmall, OS: "plan9"},
	} {
		var declareErr error
		_ = runProgram(t, func(ctx *pulumi.Context) error {
			_, declareErr = declareComputeInstance(ctx, "vm", "europe-west1", "", testNetworkName, p, nil)
			return nil
		})
		if declareErr == nil {
			t.Errorf("declareComputeInstance(%+v) = nil error, want a rejection", p)
		}
	}
}

func TestDecodeComputeInstanceProperties(t *testing.T) {
	tests := []struct {
		name    string
		props   map[string]any
		wantErr string
	}{
		{name: "valid", props: map[string]any{"size": "medium", "os": "debian-12"}},
		{
			name:    "unmapped size",
			props:   map[string]any{"size": "gigantic", "os": "debian-12"},
			wantErr: "property validation failed",
		},
		{
			name:    "unmapped os",
			props:   map[string]any{"size": "small", "os": "amazon-linux-2023"},
			wantErr: "property validation failed",
		},
		{
			// A user reaching for a raw image is asking for something the
			// schema does not offer; dropping it silently would boot a
			// different image than they named.
			name:    "raw image is rejected rather than dropped",
			props:   map[string]any{"size": "small", "os": "debian-12", "image": "my-image"},
			wantErr: "unknown or malformed property",
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
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("decode error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestResourceProgramComputeInstance(t *testing.T) {
	p := &GCPProvider{stateDir: t.TempDir(), passphrase: "test"}

	vm := func(mutate func(*spec.Resource)) spec.Resource {
		r := spec.Resource{
			ID: "build-agent", Type: spec.ResourceTypeComputeInstance, Provider: spec.ProviderGCP,
			Scope:      spec.Scope{Region: "europe-west1"},
			Properties: map[string]any{"size": "small", "os": "ubuntu-22.04"},
		}
		if mutate != nil {
			mutate(&r)
		}
		return r
	}

	t.Run("builds a program", func(t *testing.T) {
		program, region, err := p.resourceProgram(vm(nil), spec.Policies{})
		if err != nil {
			t.Fatalf("resourceProgram() error = %v", err)
		}
		if region != "europe-west1" {
			t.Errorf("region = %q, want the resource's region", region)
		}
		if program == nil {
			t.Fatal("resourceProgram() returned a nil program")
		}
	})

	t.Run("two zones are refused", func(t *testing.T) {
		_, _, err := p.resourceProgram(vm(func(r *spec.Resource) {
			r.Scope.Zones = []string{"europe-west1-a", "europe-west1-b"}
		}), spec.Policies{})
		if !errors.Is(err, compute.ErrMultipleZones) {
			t.Fatalf("resourceProgram() error = %v, want %v", err, compute.ErrMultipleZones)
		}
	})

	t.Run("decode errors propagate", func(t *testing.T) {
		program, _, err := p.resourceProgram(vm(func(r *spec.Resource) {
			r.Properties = map[string]any{}
		}), spec.Policies{})
		if err == nil {
			t.Fatal("resourceProgram() error = nil, want a decode error")
		}
		if program != nil {
			t.Error("resourceProgram() returned a program alongside an error")
		}
	})

	t.Run("exception windows are refused", func(t *testing.T) {
		sch := workWeekSchedule()
		sch.Exceptions = []schedule.Window{
			{From: "2026-12-24", To: "2027-01-06", Mode: schedule.ModeAlwaysOff},
		}
		_, _, err := p.resourceProgram(vm(nil), spec.Policies{Schedule: sch})
		if !errors.Is(err, ErrScheduleExceptionsUnsupported) {
			t.Fatalf("resourceProgram() error = %v, want %v", err, ErrScheduleExceptionsUnsupported)
		}
	})
}

func TestInstanceZone(t *testing.T) {
	tests := []struct {
		name   string
		region string
		zone   string
		want   string
	}{
		{name: "pinned zone wins", region: "europe-west1", zone: "europe-west1-c", want: "europe-west1-c"},
		{name: "derived from the region", region: "us-east1", zone: "", want: "us-east1-a"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := instanceZone(tt.region, tt.zone); got != tt.want {
				t.Errorf("instanceZone(%q, %q) = %q, want %q", tt.region, tt.zone, got, tt.want)
			}
		})
	}
}
