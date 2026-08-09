// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package spec

import (
	"fmt"
	"strings"
	"testing"
)

func validSpec() Specification {
	return Specification{
		SDDVersion: "1.0",
		Intent:     IntentDeploy,
		Resources: []Resource{
			{
				ID:         "app-db",
				Type:       ResourceTypeRelationalDatabase,
				Provider:   ProviderAgnostic,
				Properties: map[string]any{"engine": "postgres"},
			},
		},
	}
}

func TestValidate(t *testing.T) {

	tests := []struct {
		name    string
		mutate  func(s *Specification)
		wantErr bool
	}{
		{
			name:    "valid specification",
			mutate:  func(s *Specification) {},
			wantErr: false,
		},
		{
			name: "valid specification with policies",
			mutate: func(s *Specification) {
				s.Policies = Policies{AllowedRegions: []string{"eu-central-1"}}
			},
			wantErr: false,
		},
		{
			name:    "wrong sdd_version",
			mutate:  func(s *Specification) { s.SDDVersion = "2.0" },
			wantErr: true,
		},
		{
			name:    "missing sdd_version",
			mutate:  func(s *Specification) { s.SDDVersion = "" },
			wantErr: true,
		},
		{
			name:    "invalid intent",
			mutate:  func(s *Specification) { s.Intent = "annihilate" },
			wantErr: true,
		},
		{
			name:    "empty resources",
			mutate:  func(s *Specification) { s.Resources = nil },
			wantErr: true,
		},
		{
			name:    "invalid resource id with path traversal characters",
			mutate:  func(s *Specification) { s.Resources[0].ID = "../../etc/passwd" },
			wantErr: true,
		},
		{
			name:    "invalid resource id too long",
			mutate:  func(s *Specification) { s.Resources[0].ID = string(make([]byte, 64)) },
			wantErr: true,
		},
		{
			name:    "invalid resource type",
			mutate:  func(s *Specification) { s.Resources[0].Type = "nuclear_reactor" },
			wantErr: true,
		},
		{
			name:    "cross_account_role is a valid resource type",
			mutate:  func(s *Specification) { s.Resources[0].Type = ResourceTypeCrossAccountRole },
			wantErr: false,
		},
		{
			name:    "valid account reference",
			mutate:  func(s *Specification) { s.Resources[0].Account = "prod-target" },
			wantErr: false,
		},
		{
			name:    "account reference too long",
			mutate:  func(s *Specification) { s.Resources[0].Account = string(make([]byte, 65)) },
			wantErr: true,
		},
		{
			name:    "valid scope with environment and region",
			mutate:  func(s *Specification) { s.Resources[0].Scope = Scope{Environment: "staging", Region: "eu-central-1"} },
			wantErr: false,
		},
		{
			name:    "scope environment too long",
			mutate:  func(s *Specification) { s.Resources[0].Scope = Scope{Environment: string(make([]byte, 33))} },
			wantErr: true,
		},
		{
			name: "scope region and regions mutually exclusive",
			mutate: func(s *Specification) {
				s.Resources[0].Scope = Scope{Region: "eu-central-1", Regions: []string{"eu-west-1", "us-east-1"}}
			},
			wantErr: true,
		},
		{
			name:    "scope regions with a single entry rejected",
			mutate:  func(s *Specification) { s.Resources[0].Scope = Scope{Regions: []string{"eu-central-1"}} },
			wantErr: true,
		},
		{
			name: "scope regions with more than 10 entries rejected",
			mutate: func(s *Specification) {
				regions := make([]string, 11)
				for i := range regions {
					regions[i] = fmt.Sprintf("region-%d", i)
				}
				s.Resources[0].Scope = Scope{Regions: regions}
			},
			wantErr: true,
		},
		{
			name: "scope zones with more than 10 entries rejected",
			mutate: func(s *Specification) {
				zones := make([]string, 11)
				for i := range zones {
					zones[i] = fmt.Sprintf("zone-%d", i)
				}
				s.Resources[0].Scope = Scope{Zones: zones}
			},
			wantErr: true,
		},
		{
			name:    "invalid provider",
			mutate:  func(s *Specification) { s.Resources[0].Provider = "on-premise" },
			wantErr: true,
		},
		{
			name:    "nil properties",
			mutate:  func(s *Specification) { s.Resources[0].Properties = nil },
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := validSpec()
			tt.mutate(&s)
			err := Validate(&s)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestValidateBuildPipelineType covers the first addition to the
// ResourceType enum since RFC 001 (RFC 018 §3).
//
// The enum lives in two places that have to agree — the constant and the
// `oneof` tag on Resource.Type — and a new type that is declared but not
// listed validates nowhere. That is the failure this test exists to catch.
func TestValidateBuildPipelineType(t *testing.T) {
	tests := []struct {
		name    string
		typ     ResourceType
		wantErr bool
	}{
		{name: "build_pipeline", typ: ResourceTypeBuildPipeline},
		{name: "container_service", typ: ResourceTypeContainerService},
		{name: "a type that does not exist", typ: "build_pipelines", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := validSpec()
			s.Resources[0].Type = tt.typ

			err := Validate(&s)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestValidateVolumes covers the filesystem declaration of RFC 018 §2.7.
//
// Volumes sits on Resource beside Scope rather than inside Properties
// because the filesystem belongs to the scope, not to one service: two
// services in one environment sharing a name share a filesystem. That
// placement is what makes the rest of this table necessary — a field every
// resource type can spell needs someone to say which ones it means, and a
// list of mounts needs someone to say two of them cannot collide.
//
// wantMsg is set only on the rules Validate expresses in Go rather than in
// a struct tag. For those, "an error was returned" is too weak an
// assertion: every case in this table returns one, so a rule firing for
// the wrong reason would still pass.
func TestValidateVolumes(t *testing.T) {
	uploads := Volume{Name: "uploads", MountPath: "/var/lib/uploads", SizeGB: 100}

	// mountPathOf keeps the shape-of-one-volume cases to their one varying
	// field, so the table reads as a list of paths rather than of structs.
	mountPathOf := func(path string) []Volume {
		return []Volume{{Name: "uploads", MountPath: path, SizeGB: 100}}
	}

	tests := []struct {
		name    string
		typ     ResourceType
		volumes []Volume
		wantErr bool
		wantMsg string
	}{
		// The one type that mounts a filesystem, and the absence case.
		{name: "a filesystem on a container_service", typ: ResourceTypeContainerService, volumes: []Volume{uploads}},
		{name: "no filesystem at all", typ: ResourceTypeContainerService},
		{
			name:    "size omitted, left to the provider default",
			typ:     ResourceTypeContainerService,
			volumes: []Volume{{Name: "cache", MountPath: "/var/cache"}},
		},
		{
			name:    "five filesystems, the cap",
			typ:     ResourceTypeContainerService,
			volumes: volumesNamed("a", "b", "c", "d", "e"),
		},

		// The five types that are refused. Each is listed rather than
		// looped: a type moving between the two groups is a decision, and
		// it should show up as a changed line here.
		{
			name: "a relational_database cannot mount one", typ: ResourceTypeRelationalDatabase,
			volumes: []Volume{uploads}, wantErr: true, wantMsg: "cannot mount",
		},
		{
			name: "an object_storage cannot mount one", typ: ResourceTypeObjectStorage,
			volumes: []Volume{uploads}, wantErr: true, wantMsg: "cannot mount",
		},
		{
			name: "a compute_instance cannot mount one", typ: ResourceTypeComputeInstance,
			volumes: []Volume{uploads}, wantErr: true, wantMsg: "cannot mount",
		},
		{
			name: "a cross_account_role cannot mount one", typ: ResourceTypeCrossAccountRole,
			volumes: []Volume{uploads}, wantErr: true, wantMsg: "cannot mount",
		},
		// The fifth is not like the other four, and the difference is worth
		// keeping visible: a build has a filesystem, it is simply not one
		// any of the three clouds can share. Cloud Build and ACR Tasks
		// cannot mount one at all, and CodeBuild can only for a build placed
		// inside the VPC — which reverses RFC 018's deliberate choice of
		// CodeBuild's own managed network (RFC 020 §2.4).
		{
			name: "a build_pipeline cannot mount one", typ: ResourceTypeBuildPipeline,
			volumes: []Volume{uploads}, wantErr: true, wantMsg: "cannot mount",
		},

		// The shape of one volume. The mount path is the hostile field: it
		// is user-supplied and ends up as a path inside a container.
		{
			name: "missing name", typ: ResourceTypeContainerService,
			volumes: []Volume{{MountPath: "/var/lib/uploads"}}, wantErr: true,
		},
		{
			name: "a name outside the label charset", typ: ResourceTypeContainerService,
			volumes: []Volume{{Name: "../etc", MountPath: "/var/lib/uploads"}}, wantErr: true,
		},
		{
			name: "a name too long to label a filesystem", typ: ResourceTypeContainerService,
			volumes: []Volume{{Name: strings.Repeat("u", 33), MountPath: "/var/lib/uploads"}}, wantErr: true,
		},
		{
			name: "missing mount path", typ: ResourceTypeContainerService,
			volumes: []Volume{{Name: "uploads"}}, wantErr: true,
		},
		{name: "a relative mount path", typ: ResourceTypeContainerService, volumes: mountPathOf("var/lib/uploads"), wantErr: true},
		{name: "the root filesystem itself", typ: ResourceTypeContainerService, volumes: mountPathOf("/"), wantErr: true},
		{name: "a parent-directory segment", typ: ResourceTypeContainerService, volumes: mountPathOf("/var/../etc"), wantErr: true},
		{name: "a current-directory segment", typ: ResourceTypeContainerService, volumes: mountPathOf("/var/./uploads"), wantErr: true},
		{name: "an empty segment", typ: ResourceTypeContainerService, volumes: mountPathOf("/var//uploads"), wantErr: true},
		{name: "a trailing slash", typ: ResourceTypeContainerService, volumes: mountPathOf("/var/lib/uploads/"), wantErr: true},
		{name: "a space in the mount path", typ: ResourceTypeContainerService, volumes: mountPathOf("/var/lib/my uploads"), wantErr: true},
		{name: "a NUL byte in the mount path", typ: ResourceTypeContainerService, volumes: mountPathOf("/var/lib/up\x00loads"), wantErr: true},
		{
			name: "a mount path too long to be one", typ: ResourceTypeContainerService,
			volumes: mountPathOf("/" + strings.Repeat("var/", 79) + "var"), wantErr: true,
		},
		{
			name: "a negative size", typ: ResourceTypeContainerService,
			volumes: []Volume{{Name: "uploads", MountPath: "/var/lib/uploads", SizeGB: -1}}, wantErr: true,
		},
		{
			name: "a size beyond the cap", typ: ResourceTypeContainerService,
			volumes: []Volume{{Name: "uploads", MountPath: "/var/lib/uploads", SizeGB: 65537}}, wantErr: true,
		},

		// Collisions within one list. Both are Specifications that cannot
		// be honoured rather than merely odd ones.
		{
			name: "two filesystems under one name", typ: ResourceTypeContainerService,
			volumes: []Volume{
				{Name: "uploads", MountPath: "/var/lib/uploads"},
				{Name: "uploads", MountPath: "/var/lib/other"},
			},
			wantErr: true, wantMsg: "declared twice",
		},
		{
			name: "two filesystems at one mount path", typ: ResourceTypeContainerService,
			volumes: []Volume{
				{Name: "uploads", MountPath: "/var/lib/shared"},
				{Name: "cache", MountPath: "/var/lib/shared"},
			},
			wantErr: true, wantMsg: "mount path",
		},
		{
			name: "six filesystems, one past the cap", typ: ResourceTypeContainerService,
			volumes: volumesNamed("a", "b", "c", "d", "e", "f"), wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := validSpec()
			s.Resources[0].Type = tt.typ
			s.Resources[0].Volumes = tt.volumes

			err := Validate(&s)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("Validate() error = %v, want it to mention %q", err, tt.wantMsg)
			}
		})
	}
}

// volumesNamed builds a list of distinct volumes, one per name, for the
// cases that are about the length of the list and nothing else.
func volumesNamed(names ...string) []Volume {
	volumes := make([]Volume, 0, len(names))
	for _, name := range names {
		volumes = append(volumes, Volume{Name: name, MountPath: "/var/lib/" + name})
	}
	return volumes
}
