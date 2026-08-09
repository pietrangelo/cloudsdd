// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package pipeline

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
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

// errDecoderRefused stands in for whatever a provider's decode.Decoder
// reports. Its message deliberately carries no prefix, so a test can tell
// whether DecodeAndValidate added one of its own.
var errDecoderRefused = errors.New("aws: unknown or malformed property")

// decodeStrictly stands in for decode.Decoder.Properties at the one thing
// DecodeAndValidate depends on: a strict decode into out.
//
// The real decoder is not imported, and cannot be — internal/provider/decode
// imports go-playground/validator and this package must keep the zero
// internal imports RFC 019 §2.1 protects. That constraint is why the
// parameter is a plain func in the first place, so exercising it with one
// here is the honest test of the seam rather than a workaround.
func decodeStrictly(props map[string]any, out any) error {
	raw, err := json.Marshal(props)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(out)
}

// refuseToDecode is the decoder that rejects everything.
func refuseToDecode(map[string]any, any) error { return errDecoderRefused }

// TestDecodeAndValidate covers the three steps every provider performed
// identically before RFC 019 §2.2 collapsed them into one function: decode,
// Validate, and the Dockerfile generation that refuses an unbuildable stack
// at plan time.
//
// Every failure is checked for exactly one leading prefix. It is how a user
// finds which provider refused their resource (RFC 019 §4), and both ways of
// getting it wrong are live: a shared helper can drop the prefix on a rule it
// now owns, or add a second one to an error the decoder had already labelled.
func TestDecodeAndValidate(t *testing.T) {
	httpsSource := map[string]any{"repository": "https://github.com/acme/api", "revision": "main"}
	goStack := map[string]any{"runtime": "go", "version": "1.22"}

	tests := []struct {
		name      string
		source    map[string]any
		stack     map[string]any
		imageName string
		decode    func(map[string]any, any) error // nil selects decodeStrictly
		prefix    string
		want      *BuildPipelineProperties
		wantErr   error
	}{
		{
			name:      "a complete pipeline",
			source:    httpsSource,
			stack:     goStack,
			imageName: "acme/api",
			prefix:    "aws",
			want: &BuildPipelineProperties{
				Source:    Source{Repository: "https://github.com/acme/api", Revision: "main"},
				Stack:     Stack{Runtime: RuntimeGo, Version: "1.22"},
				ImageName: "acme/api",
				Ports:     []int{8080},
			},
		},
		{
			// The decoder's error is returned as it stands. It has already
			// named the provider, and wrapping it would say so twice.
			name:      "the decoder refuses the properties",
			source:    httpsSource,
			stack:     goStack,
			imageName: "acme/api",
			decode:    refuseToDecode,
			prefix:    "aws",
			wantErr:   errDecoderRefused,
		},
		{
			// Validate's rules are the shared ones (RFC 018 §2.6), so they
			// have to reach the user carrying the provider that applied them.
			name:      "an unencrypted repository",
			source:    map[string]any{"repository": "http://github.com/acme/api", "revision": "main"},
			stack:     goStack,
			imageName: "acme/api",
			prefix:    "gcp",
			wantErr:   ErrRepositoryScheme,
		},
		{
			name:      "an image name that names a registry",
			source:    httpsSource,
			stack:     goStack,
			imageName: "ghcr.io/acme/api",
			prefix:    "azure",
			wantErr:   ErrImageNameMalformed,
		},
		{
			// The reason generation runs here at all: an unpinned version is
			// refused while the user is still reading a plan, rather than by
			// a build that has already been provisioned and started.
			name:      "a runtime version with no pinned base image",
			source:    httpsSource,
			stack:     map[string]any{"runtime": "go", "version": "1.21"},
			imageName: "acme/api",
			prefix:    "azure",
			wantErr:   ErrUnsupportedVersion,
		},
		{
			// The struct tag catches this first in production, where the real
			// decoder validates `oneof`. It is still refused here, because a
			// runtime with no template is a Dockerfile that cannot be written.
			name:      "a runtime outside the enum",
			source:    httpsSource,
			stack:     map[string]any{"runtime": "rust", "version": "1.79"},
			imageName: "acme/api",
			prefix:    "gcp",
			wantErr:   ErrUnsupportedRuntime,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			props := map[string]any{
				"source":     tt.source,
				"stack":      tt.stack,
				"image_name": tt.imageName,
				"ports":      []any{8080},
			}
			decode := tt.decode
			if decode == nil {
				decode = decodeStrictly
			}

			got, err := DecodeAndValidate(props, decode, tt.prefix)

			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("DecodeAndValidate() = %v, want nil", err)
				}
				if !reflect.DeepEqual(got, tt.want) {
					t.Fatalf("DecodeAndValidate() = %+v, want %+v", got, tt.want)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("DecodeAndValidate() = %v, want %v", err, tt.wantErr)
			}
			// A refused pipeline yields no properties: a caller that ignored
			// the error must not find something usable to act on.
			if got != nil {
				t.Errorf("DecodeAndValidate() = %+v with an error, want nil", got)
			}
			label := tt.prefix + ": "
			if !strings.HasPrefix(err.Error(), label) {
				t.Errorf("error %q does not open with %q", err, label)
			}
			if n := strings.Count(err.Error(), label); n != 1 {
				t.Errorf("error %q names %q %d times, want once", err, tt.prefix, n)
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
