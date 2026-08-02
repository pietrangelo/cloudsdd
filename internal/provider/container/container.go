// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

// Package container holds the cloud-agnostic shape of a container_service
// resource: the properties every provider decodes, and the image rules
// they all apply (RFC 017 §2.2, §2.4).
//
// The struct lives here rather than being redeclared per provider for the
// reason RFC 011 §1.1H recorded and RFC 013 followed: three copies of a
// schema drift, and a drifted schema means a Specification that is valid
// on one cloud and silently different on another. Each provider still
// decodes through its own decode.Decoder, so error prefixes stay where
// they belong; only the shape is shared.
//
// What is deliberately *not* shared is the mapping from Size onto CPU and
// memory. Fargate, Cloud Run and Container Apps quantise those
// differently, so each package owns its own table.
package container

import (
	"errors"
	"fmt"
	"strings"
)

// Size is the cloud-agnostic amount of machine one replica gets. The user
// says how much they need; each provider picks the CPU/memory pair that
// expresses it, the same way nobody writes "db.t3.micro" in a
// Specification today.
type Size string

const (
	SizeSmall  Size = "small"
	SizeMedium Size = "medium"
	SizeLarge  Size = "large"
)

// Replica bounds (RFC 017 §4). Ten is not a capacity judgement; it is the
// ceiling that keeps a mistranslated prompt from requesting an unbounded
// fleet. Zero is legal and means "deployed, running nothing" — the same
// state a power schedule puts the service in outside working hours.
const (
	defaultReplicas = 1
	MaxReplicas     = 10
)

// Properties represents the Properties of a Resource with Type
// "container_service" on any provider (RFC 017 §2.2).
//
// Replicas is *int and Public is *bool, not their value types, so absence
// activates the default rather than the zero value — the tri-state pattern
// RFC 011 §2.5 established for every security-relevant property. It
// matters more here than elsewhere: an absent Replicas means one, while an
// explicit zero means none, and a plain int could not tell them apart.
//
// There is deliberately no environment or configuration field. Anything
// shaped like `env` is where credentials get pasted, and offering it
// without a secrets story would be offering the paste. Deferred to the RFC
// that can pair the two (RFC 017 §2.2).
type Properties struct {
	// Image is the container image reference. Constrained by ValidateImage
	// rather than by a validator tag: the rule depends on
	// Policies.AllowedRegistries, which a struct tag cannot see.
	Image string `json:"image" validate:"required,max=512"`

	// Port is the port the container listens on. It is never opened to the
	// internet directly — ingress reaches it through the platform's load
	// balancer, and nothing else can (RFC 017 §2.3).
	Port int `json:"port" validate:"required,min=1,max=65535"`

	Size Size `json:"size" validate:"required,oneof=small medium large"`

	Replicas *int `json:"replicas,omitempty" validate:"omitempty,min=0,max=10"`

	// Public puts the service behind a managed HTTPS ingress. It never
	// opens the container's own port: the platform terminates TLS and the
	// container is reachable only from that ingress.
	Public *bool `json:"public,omitempty"`
}

// EffectiveReplicas returns Replicas, or the default when absent.
//
// An explicit zero is returned as zero: a Specification may legitimately
// declare a service that is deployed and running nothing.
func (p Properties) EffectiveReplicas() int {
	if p.Replicas == nil {
		return defaultReplicas
	}
	return *p.Replicas
}

// EffectivePublic reports whether internet ingress was explicitly
// requested. Absent means false: the one resource type that runs arbitrary
// user code is not the one exposed to the internet by default (RFC 017
// §2.3).
func (p Properties) EffectivePublic() bool {
	return p.Public != nil && *p.Public
}

// Image reference errors (RFC 017 §2.4).
var (
	// ErrImageMalformed covers a reference no registry would accept.
	ErrImageMalformed = errors.New("container image reference is malformed")

	// ErrImageMutable indicates a tag where a digest was required. A tag
	// can be repointed after the Specification was reviewed, so what runs
	// is not what was approved.
	ErrImageMutable = errors.New("container image must be pinned to a digest unless its registry is allow-listed")

	// ErrImageLatest indicates the `latest` tag, explicit or implied. It is
	// refused with or without an allowlist: nothing that reads "deploy
	// whatever is newest, forever" belongs in a reviewed artifact.
	ErrImageLatest = errors.New("container image must not use the `latest` tag")

	// ErrRegistryNotAllowed indicates a registry outside
	// policies.allowed_registries.
	ErrRegistryNotAllowed = errors.New("container image registry not in allowed_registries")
)

// DefaultRegistry is where an image with no registry component comes from.
// Docker's own convention, reproduced here because the rules below have to
// name a registry even when the reference does not.
const DefaultRegistry = "docker.io"

// digestAlgorithm is the only digest form accepted. Registries support
// others in principle; every registry in practice uses this one, and a
// list of one is easier to reason about than a parser that accepts forms
// nothing produces.
const digestAlgorithm = "sha256"

// digestHexLength is the length of a sha256 digest in hex.
const digestHexLength = 64

// mutableTag is the tag refused outright.
const mutableTag = "latest"

// Reference is a parsed container image reference.
type Reference struct {
	// Registry is the host the image is pulled from, defaulted to
	// DefaultRegistry when the reference names none.
	Registry string
	// Repository is the path within the registry.
	Repository string
	// Tag is the mutable label, empty when the reference carries none.
	Tag string
	// Digest is the immutable content address ("sha256:..."), empty when
	// the reference carries none.
	Digest string
}

// Pinned reports whether the reference names an immutable artifact.
func (r Reference) Pinned() bool { return r.Digest != "" }

// ParseImage splits an image reference into its parts.
//
// It is deliberately stricter than a registry would be. This is the one
// property whose value decides what code runs, so a reference that parses
// two ways must not parse at all — an input that is ambiguous here is an
// input whose meaning is decided by whichever cloud receives it.
func ParseImage(image string) (Reference, error) {
	if image == "" {
		return Reference{}, fmt.Errorf("%w: empty", ErrImageMalformed)
	}
	if strings.ContainsFunc(image, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return Reference{}, fmt.Errorf("%w: %q contains whitespace or control characters", ErrImageMalformed, image)
	}

	name := image
	var digest string
	if at := strings.Index(name, "@"); at >= 0 {
		name, digest = name[:at], name[at+1:]
		if err := validateDigest(digest); err != nil {
			return Reference{}, err
		}
	}

	// The tag separator is the last colon that follows the last slash.
	// Anything earlier is a registry port: "localhost:5000/app" names a
	// host and no tag, and splitting on the first colon would read the
	// whole path as one.
	var tag string
	if colon := strings.LastIndex(name, ":"); colon > strings.LastIndex(name, "/") {
		name, tag = name[:colon], name[colon+1:]
		if tag == "" {
			return Reference{}, fmt.Errorf("%w: %q has an empty tag", ErrImageMalformed, image)
		}
	}
	if name == "" {
		return Reference{}, fmt.Errorf("%w: %q names no repository", ErrImageMalformed, image)
	}

	// A leading component is a registry only if it looks like a host: it
	// carries a dot or a port, or is localhost. Otherwise it is the first
	// path element of a Docker Hub repository ("library/nginx"), which is
	// the convention every registry client follows.
	registry := DefaultRegistry
	repository := name
	if slash := strings.Index(name, "/"); slash > 0 {
		if head := name[:slash]; strings.ContainsAny(head, ".:") || head == "localhost" {
			registry, repository = head, name[slash+1:]
		}
	}
	if repository == "" {
		return Reference{}, fmt.Errorf("%w: %q names no repository", ErrImageMalformed, image)
	}

	return Reference{
		Registry:   registry,
		Repository: repository,
		Tag:        tag,
		Digest:     digest,
	}, nil
}

// validateDigest checks the content address is one a registry could
// resolve. A malformed digest is worse than no digest: it looks pinned.
func validateDigest(digest string) error {
	algorithm, hex, ok := strings.Cut(digest, ":")
	if !ok || algorithm != digestAlgorithm {
		return fmt.Errorf("%w: digest %q is not %s:...", ErrImageMalformed, digest, digestAlgorithm)
	}
	if len(hex) != digestHexLength {
		return fmt.Errorf("%w: digest %q is %d hex characters, want %d",
			ErrImageMalformed, digest, len(hex), digestHexLength)
	}
	for _, r := range hex {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return fmt.Errorf("%w: digest %q is not lowercase hex", ErrImageMalformed, digest)
		}
	}
	return nil
}

// ValidateImage applies RFC 017 §2.4's two rules to an image reference.
//
// They are two independent axes, and reading them as one is the mistake
// worth naming. The **digest** rule governs *mutability*: what runs must be
// what was reviewed. The **allowlist** governs *origin*: the artifact must
// come from somewhere the operator named. So:
//
//   - `latest`, explicit or implied by an absent tag, is refused always;
//   - a digest satisfies the mutability rule from any registry;
//   - a tag satisfies it only from an allow-listed registry, because an
//     operator who named a registry has taken responsibility for what its
//     tags point at;
//   - when an allowlist exists it constrains *every* image, digest or not.
//
// That last clause is a deliberate reading of RFC 017 §2.4, whose prose
// ("a digest is accepted from anywhere") would otherwise let any digest
// bypass the allowlist entirely — which would make the §4 threat-table row
// about attacker-controlled registries name a mitigation that does not
// mitigate. An allowlist that anything can step around is not a policy.
func ValidateImage(image string, allowedRegistries []string) error {
	ref, err := ParseImage(image)
	if err != nil {
		return err
	}

	if len(allowedRegistries) > 0 && !registryAllowed(ref.Registry, allowedRegistries) {
		return fmt.Errorf("image %q comes from registry %q, not in allowed_registries %v: %w",
			image, ref.Registry, allowedRegistries, ErrRegistryNotAllowed)
	}

	// An absent tag means `latest` to every registry client, so it is
	// refused as `latest` rather than as a missing tag — unless a digest
	// pins the reference, in which case the tag is decoration.
	if !ref.Pinned() && (ref.Tag == "" || ref.Tag == mutableTag) {
		return fmt.Errorf("image %q resolves to `%s`: %w", image, mutableTag, ErrImageLatest)
	}
	if ref.Tag == mutableTag {
		return fmt.Errorf("image %q names the `%s` tag: %w", image, mutableTag, ErrImageLatest)
	}

	if !ref.Pinned() && !registryAllowed(ref.Registry, allowedRegistries) {
		return fmt.Errorf("image %q is tagged rather than pinned and %q is not allow-listed: %w",
			image, ref.Registry, ErrImageMutable)
	}
	return nil
}

// registryAllowed reports whether registry appears in allowed. An empty
// allowed list means nothing is allow-listed, which is what makes the
// digest requirement the default rather than the exception.
func registryAllowed(registry string, allowed []string) bool {
	for _, a := range allowed {
		if strings.EqualFold(a, registry) {
			return true
		}
	}
	return false
}
