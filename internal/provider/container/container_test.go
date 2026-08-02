// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package container

import (
	"errors"
	"strings"
	"testing"
)

// TestParseImage covers the reference grammar. The cases that matter are
// the ambiguous ones: a registry port and a tag are both a colon, and
// reading one as the other silently changes which host is contacted.
func TestParseImage(t *testing.T) {
	tests := []struct {
		name       string
		image      string
		registry   string
		repository string
		tag        string
		digest     string
		wantErr    error
	}{
		{
			name:       "bare repository defaults to docker hub",
			image:      "nginx:1.27",
			registry:   DefaultRegistry,
			repository: "nginx",
			tag:        "1.27",
		},
		{
			name:       "namespaced repository is not a registry",
			image:      "library/nginx:1.27",
			registry:   DefaultRegistry,
			repository: "library/nginx",
			tag:        "1.27",
		},
		{
			name:       "a dotted head is a registry",
			image:      "ghcr.io/acme/api:2.1",
			registry:   "ghcr.io",
			repository: "acme/api",
			tag:        "2.1",
		},
		{
			name:       "a port makes the head a registry, not a tag",
			image:      "localhost:5000/api:2.1",
			registry:   "localhost:5000",
			repository: "api",
			tag:        "2.1",
		},
		{
			name:       "localhost is a registry without a dot",
			image:      "localhost/api:2.1",
			registry:   "localhost",
			repository: "api",
			tag:        "2.1",
		},
		{
			name:       "a port with no tag is still a registry",
			image:      "registry.internal:5000/api@sha256:" + strings.Repeat("a", 64),
			registry:   "registry.internal:5000",
			repository: "api",
			digest:     "sha256:" + strings.Repeat("a", 64),
		},
		{
			name:       "a digest alongside a tag keeps both",
			image:      "ghcr.io/acme/api:2.1@sha256:" + strings.Repeat("b", 64),
			registry:   "ghcr.io",
			repository: "acme/api",
			tag:        "2.1",
			digest:     "sha256:" + strings.Repeat("b", 64),
		},
		{
			name:       "no tag and no digest parses, and is refused later",
			image:      "ghcr.io/acme/api",
			registry:   "ghcr.io",
			repository: "acme/api",
		},
		{name: "empty", image: "", wantErr: ErrImageMalformed},
		{name: "whitespace", image: "ghcr.io/acme/api :2.1", wantErr: ErrImageMalformed},
		{name: "newline injected", image: "ghcr.io/acme/api:2.1\n", wantErr: ErrImageMalformed},
		{name: "empty tag", image: "ghcr.io/acme/api:", wantErr: ErrImageMalformed},
		{name: "no repository", image: "@sha256:" + strings.Repeat("a", 64), wantErr: ErrImageMalformed},
		{
			name:    "digest with the wrong algorithm",
			image:   "ghcr.io/acme/api@md5:" + strings.Repeat("a", 32),
			wantErr: ErrImageMalformed,
		},
		{
			name:    "digest of the wrong length",
			image:   "ghcr.io/acme/api@sha256:" + strings.Repeat("a", 63),
			wantErr: ErrImageMalformed,
		},
		{
			name:    "digest that is not hex looks pinned and is not",
			image:   "ghcr.io/acme/api@sha256:" + strings.Repeat("z", 64),
			wantErr: ErrImageMalformed,
		},
		{
			name:    "uppercase hex is rejected rather than folded",
			image:   "ghcr.io/acme/api@sha256:" + strings.Repeat("A", 64),
			wantErr: ErrImageMalformed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref, err := ParseImage(tt.image)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("ParseImage(%q) error = %v, want %v", tt.image, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseImage(%q) returned %v, want a reference", tt.image, err)
			}
			if ref.Registry != tt.registry {
				t.Errorf("registry = %q, want %q", ref.Registry, tt.registry)
			}
			if ref.Repository != tt.repository {
				t.Errorf("repository = %q, want %q", ref.Repository, tt.repository)
			}
			if ref.Tag != tt.tag {
				t.Errorf("tag = %q, want %q", ref.Tag, tt.tag)
			}
			if ref.Digest != tt.digest {
				t.Errorf("digest = %q, want %q", ref.Digest, tt.digest)
			}
			if got := ref.Pinned(); got != (tt.digest != "") {
				t.Errorf("Pinned() = %v, want %v", got, tt.digest != "")
			}
		})
	}
}

// TestValidateImage is the heart of RFC 017 §2.4. Two independent axes:
// a digest governs whether the artifact can change after review, and the
// allowlist governs where it comes from.
func TestValidateImage(t *testing.T) {
	const digest = "@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	tests := []struct {
		name    string
		image   string
		allowed []string
		wantErr error
	}{
		{
			name:  "a digest is accepted with no allowlist",
			image: "ghcr.io/acme/api" + digest,
		},
		{
			name:    "a tag is refused with no allowlist",
			image:   "ghcr.io/acme/api:2.1",
			wantErr: ErrImageMutable,
		},
		{
			name:    "a tag is accepted from an allow-listed registry",
			image:   "ghcr.io/acme/api:2.1",
			allowed: []string{"ghcr.io"},
		},
		{
			name:    "a tag is refused from a registry outside the allowlist",
			image:   "evil.example/acme/api:2.1",
			allowed: []string{"ghcr.io"},
			wantErr: ErrRegistryNotAllowed,
		},
		{
			// The clause ValidateImage documents as a deliberate reading of
			// §2.4: an allowlist an attacker steps around with a digest is
			// not a policy.
			name:    "a digest does not bypass an allowlist",
			image:   "evil.example/acme/api" + digest,
			allowed: []string{"ghcr.io"},
			wantErr: ErrRegistryNotAllowed,
		},
		{
			name:    "latest is refused with no allowlist",
			image:   "ghcr.io/acme/api:latest",
			wantErr: ErrImageLatest,
		},
		{
			name:    "latest is refused from an allow-listed registry too",
			image:   "ghcr.io/acme/api:latest",
			allowed: []string{"ghcr.io"},
			wantErr: ErrImageLatest,
		},
		{
			name:    "latest is refused even when pinned, because the tag is a lie",
			image:   "ghcr.io/acme/api:latest" + digest,
			allowed: []string{"ghcr.io"},
			wantErr: ErrImageLatest,
		},
		{
			name:    "an absent tag means latest",
			image:   "ghcr.io/acme/api",
			allowed: []string{"ghcr.io"},
			wantErr: ErrImageLatest,
		},
		{
			name:    "an absent tag is latest with no allowlist too",
			image:   "ghcr.io/acme/api",
			wantErr: ErrImageLatest,
		},
		{
			name:    "the allowlist matches case-insensitively, as hostnames do",
			image:   "GHCR.IO/acme/api:2.1",
			allowed: []string{"ghcr.io"},
		},
		{
			name:    "docker hub must be named as docker.io to be allow-listed",
			image:   "nginx:1.27",
			allowed: []string{"docker.io"},
		},
		{
			name:    "an unqualified image is not allow-listed by naming another registry",
			image:   "nginx:1.27",
			allowed: []string{"ghcr.io"},
			wantErr: ErrRegistryNotAllowed,
		},
		{
			name:    "a malformed reference fails before any policy applies",
			image:   "ghcr.io/acme/api@sha256:short",
			allowed: []string{"ghcr.io"},
			wantErr: ErrImageMalformed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateImage(tt.image, tt.allowed)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("ValidateImage(%q, %v) = %v, want nil", tt.image, tt.allowed, err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ValidateImage(%q, %v) = %v, want %v", tt.image, tt.allowed, err, tt.wantErr)
			}
		})
	}
}

// TestEffectiveReplicas: absent means one, and an explicit zero must
// survive as zero. A plain int could not tell the two apart, which is why
// the field is a pointer — and a service scheduled off is exactly the
// deployed-but-running-nothing state zero expresses.
func TestEffectiveReplicas(t *testing.T) {
	zero, one, ten := 0, 1, 10

	tests := []struct {
		name     string
		replicas *int
		want     int
	}{
		{name: "absent defaults to one", replicas: nil, want: defaultReplicas},
		{name: "explicit zero stays zero", replicas: &zero, want: 0},
		{name: "explicit one", replicas: &one, want: 1},
		{name: "the ceiling", replicas: &ten, want: MaxReplicas},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (Properties{Replicas: tt.replicas}).EffectiveReplicas(); got != tt.want {
				t.Errorf("EffectiveReplicas() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestEffectivePublic: absent means private. The one resource type that
// runs arbitrary user code is not the one exposed by default.
func TestEffectivePublic(t *testing.T) {
	yes, no := true, false

	tests := []struct {
		name   string
		public *bool
		want   bool
	}{
		{name: "absent is private", public: nil, want: false},
		{name: "explicit false", public: &no, want: false},
		{name: "explicit true", public: &yes, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (Properties{Public: tt.public}).EffectivePublic(); got != tt.want {
				t.Errorf("EffectivePublic() = %v, want %v", got, tt.want)
			}
		})
	}
}

// FuzzParseImage asserts the parser's invariants against arbitrary input.
//
// The property that matters is not "does not panic" — it is that anything
// ParseImage accepts is a reference whose parts recompose into something
// naming the same registry. This is the one property in the schema whose
// value decides what code runs, so a reference that parses into a registry
// the user did not write is the worst failure this package can have.
func FuzzParseImage(f *testing.F) {
	for _, seed := range []string{
		"nginx:1.27",
		"ghcr.io/acme/api:2.1",
		"localhost:5000/api",
		"ghcr.io/acme/api@sha256:" + strings.Repeat("a", 64),
		"",
		":",
		"@",
		"a/b/c:d:e",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, image string) {
		ref, err := ParseImage(image)
		if err != nil {
			return
		}

		if ref.Registry == "" || ref.Repository == "" {
			t.Fatalf("ParseImage(%q) accepted a reference with an empty registry or repository: %+v", image, ref)
		}
		// A registry that is not a substring of the input is a registry the
		// user never named — the parser inventing a host to pull from.
		if ref.Registry != DefaultRegistry && !strings.Contains(image, ref.Registry) {
			t.Fatalf("ParseImage(%q) produced registry %q, which does not appear in the input", image, ref.Registry)
		}
		if ref.Digest != "" {
			if err := validateDigest(ref.Digest); err != nil {
				t.Fatalf("ParseImage(%q) accepted digest %q that validateDigest rejects: %v",
					image, ref.Digest, err)
			}
		}
		// Idempotence: a reference that parsed must survive being written
		// back out and parsed again, or two halves of the system reading
		// the same Specification could disagree about the artifact.
		round := ref.Registry + "/" + ref.Repository
		if ref.Tag != "" {
			round += ":" + ref.Tag
		}
		if ref.Digest != "" {
			round += "@" + ref.Digest
		}
		again, err := ParseImage(round)
		if err != nil {
			t.Fatalf("ParseImage(%q) recomposed to %q, which does not parse: %v", image, round, err)
		}
		if again != ref {
			t.Fatalf("ParseImage(%q) = %+v, but its recomposition %q parses to %+v", image, ref, round, again)
		}
	})
}

// TestValidateIngress covers the one cross-field rule that holds on every
// provider (RFC 017 §2.3.1).
//
// Only this half is shared. "A public service needs a domain" is an AWS
// limitation — Cloud Run is public without one — so it lives in the AWS
// provider, where RFC 012 §1.3's rule applies: a request a provider cannot
// express is refused by that provider, and by name.
func TestValidateIngress(t *testing.T) {
	yes, no := true, false

	tests := []struct {
		name    string
		domain  string
		public  *bool
		wantErr error
	}{
		{name: "no domain, private", public: nil},
		{name: "no domain, public", public: &yes},
		{name: "domain and public", domain: "api.acme.example", public: &yes},
		{
			// A hostname on a service nothing outside can reach is a
			// property the user asked for and will not get.
			name:    "domain without public",
			domain:  "api.acme.example",
			public:  nil,
			wantErr: ErrDomainWithoutPublic,
		},
		{
			name:    "domain with public explicitly false",
			domain:  "api.acme.example",
			public:  &no,
			wantErr: ErrDomainWithoutPublic,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Properties{Domain: tt.domain, Public: tt.public}.ValidateIngress()

			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("ValidateIngress() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ValidateIngress() = %v, want %v", err, tt.wantErr)
			}
			// The message has to carry the domain, or an operator with
			// several services cannot tell which one was refused.
			if !strings.Contains(err.Error(), tt.domain) {
				t.Errorf("error %q does not name the domain", err)
			}
		})
	}
}
