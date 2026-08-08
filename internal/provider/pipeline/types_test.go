// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package pipeline

import (
	"errors"
	"strings"
	"testing"
)

// TestSourceValidate covers the two rules that keep a build honest about
// where its code came from: the transport is authenticated, and the URL
// carries no credential (RFC 018 §2.6).
func TestSourceValidate(t *testing.T) {
	tests := []struct {
		name    string
		source  Source
		wantErr error
	}{
		{name: "an https repository and a branch", source: Source{Repository: "https://github.com/acme/api", Revision: "main"}},
		{name: "a tag", source: Source{Repository: "https://github.com/acme/api", Revision: "v1.4.0"}},
		{name: "a commit sha", source: Source{Repository: "https://github.com/acme/api.git", Revision: strings.Repeat("a", 40)}},
		{name: "a namespaced branch", source: Source{Repository: "https://gitlab.com/acme/group/api", Revision: "release/2026-08"}},
		{
			// An unencrypted clone builds whatever the network returned.
			name:    "an http repository",
			source:  Source{Repository: "http://github.com/acme/api", Revision: "main"},
			wantErr: ErrRepositoryScheme,
		},
		{
			name:    "a git:// repository",
			source:  Source{Repository: "git://github.com/acme/api", Revision: "main"},
			wantErr: ErrRepositoryScheme,
		},
		{
			// Refused as malformed rather than as a scheme: url.Parse
			// cannot read the scp form at all. Which error it is matters
			// less than that no non-HTTPS remote gets through.
			name:    "an scp-style ssh remote",
			source:  Source{Repository: "git@github.com:acme/api.git", Revision: "main"},
			wantErr: ErrRepositoryMalformed,
		},
		{
			// The credential this schema has no property for, smuggled in
			// through the one string that accepts arbitrary text.
			name:    "a token in the userinfo",
			source:  Source{Repository: "https://oauth2:ghp_secret@github.com/acme/api", Revision: "main"},
			wantErr: ErrRepositoryCredential,
		},
		{
			name:    "a username with no password",
			source:  Source{Repository: "https://someone@github.com/acme/api", Revision: "main"},
			wantErr: ErrRepositoryCredential,
		},
		{
			name:    "no host",
			source:  Source{Repository: "https:///acme/api", Revision: "main"},
			wantErr: ErrRepositoryMalformed,
		},
		{
			name:    "no repository path",
			source:  Source{Repository: "https://github.com/", Revision: "main"},
			wantErr: ErrRepositoryMalformed,
		},
		{
			// The revision reaches a managed build service as an argument.
			name:    "a revision that opens a flag",
			source:  Source{Repository: "https://github.com/acme/api", Revision: "--upload-pack=touch /tmp/pwn"},
			wantErr: ErrRevisionMalformed,
		},
		{
			name:    "a revision with a shell metacharacter",
			source:  Source{Repository: "https://github.com/acme/api", Revision: "main; rm -rf /"},
			wantErr: ErrRevisionMalformed,
		},
		{
			// Git refuses `..` in a refname, and so does this: a revision
			// that reads as a range is a revision with two meanings.
			name:    "a revision containing a range",
			source:  Source{Repository: "https://github.com/acme/api", Revision: "main..prod"},
			wantErr: ErrRevisionMalformed,
		},
		{
			name:    "an empty revision",
			source:  Source{Repository: "https://github.com/acme/api", Revision: ""},
			wantErr: ErrRevisionMalformed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.source.Validate()

			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tt.wantErr)
			}
			// A rejected credential must not be echoed back into a log line.
			if errors.Is(err, ErrRepositoryCredential) && strings.Contains(err.Error(), "ghp_secret") {
				t.Errorf("error %q leaks the credential it refused", err)
			}
		})
	}
}

// TestBuildPipelinePropertiesValidate covers the image name, which is the
// one field that has to satisfy a registry's grammar rather than ours.
func TestBuildPipelinePropertiesValidate(t *testing.T) {
	valid := Source{Repository: "https://github.com/acme/api", Revision: "main"}

	tests := []struct {
		name      string
		imageName string
		source    Source
		wantErr   error
	}{
		{name: "a bare name", imageName: "api", source: valid},
		{name: "a namespaced name", imageName: "acme/api", source: valid},
		{name: "separators", imageName: "acme/api-service_v2.core", source: valid},
		{name: "uppercase", imageName: "Acme/API", source: valid, wantErr: ErrImageNameMalformed},
		{name: "a leading slash", imageName: "/acme/api", source: valid, wantErr: ErrImageNameMalformed},
		{name: "a trailing separator", imageName: "acme/api-", source: valid, wantErr: ErrImageNameMalformed},
		{name: "a path traversal", imageName: "acme/../api", source: valid, wantErr: ErrImageNameMalformed},
		{name: "a registry host", imageName: "ghcr.io/acme/api", source: valid, wantErr: ErrImageNameMalformed},
		{name: "empty", imageName: "", source: valid, wantErr: ErrImageNameMalformed},
		{
			// The source is checked first: a bad repository is reported as
			// one, not as whatever the next rule happens to notice.
			name:      "a bad source and a good name",
			imageName: "acme/api",
			source:    Source{Repository: "http://github.com/acme/api", Revision: "main"},
			wantErr:   ErrRepositoryScheme,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := BuildPipelineProperties{
				Source:    tt.source,
				Stack:     Stack{Runtime: RuntimeGo, Version: "1.22"},
				ImageName: tt.imageName,
			}
			err := p.Validate()

			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestEffectiveRetain(t *testing.T) {
	one, hundred := 1, MaxRetain

	tests := []struct {
		name   string
		retain *int
		want   int
	}{
		{name: "absent takes the default", retain: nil, want: DefaultRetain},
		{name: "explicit one", retain: &one, want: 1},
		{name: "explicit ceiling", retain: &hundred, want: MaxRetain},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (BuildPipelineProperties{Retain: tt.retain}).EffectiveRetain(); got != tt.want {
				t.Errorf("EffectiveRetain() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestDeclaresPort covers the agreement a container_service is held to
// (RFC 018 §2.3). An empty list declares nothing, so nothing agrees with
// it — a service pointed at a pipeline that names no ports is a deployment
// that starts and answers nothing.
func TestDeclaresPort(t *testing.T) {
	tests := []struct {
		name  string
		ports []int
		port  int
		want  bool
	}{
		{name: "the only declared port", ports: []int{8080}, port: 8080, want: true},
		{name: "one of several", ports: []int{8080, 9090}, port: 9090, want: true},
		{name: "an undeclared port", ports: []int{8080}, port: 9090},
		{name: "no ports declared", ports: nil, port: 8080},
		{name: "zero is never declared", ports: []int{8080}, port: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (BuildPipelineProperties{Ports: tt.ports}).DeclaresPort(tt.port); got != tt.want {
				t.Errorf("DeclaresPort(%d) = %v, want %v", tt.port, got, tt.want)
			}
		})
	}
}
