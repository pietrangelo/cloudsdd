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
	//
	// Optional since RFC 018 §3, because Pipeline is the other way to say
	// where the image comes from. Exactly one of the two is required;
	// ValidateSource is what enforces that, since no struct tag can.
	Image string `json:"image,omitempty" validate:"omitempty,max=512"`

	// Pipeline names a build_pipeline in the same Specification whose
	// output this service runs (RFC 018 §2.9). It is the schema's first
	// cross-resource reference, and it is resolved at the Engine — the
	// provider receives the digest the build produced, never the name.
	//
	// A pointer rather than a plain string so that an explicitly empty
	// `"pipeline": ""` is distinguishable from an absent one: the first is
	// a reference the user meant to write and got wrong, and it deserves a
	// better error than "name an image or a pipeline".
	// Bounded to a resource ID's length; that the name resolves to a
	// build_pipeline that exists is the Engine's business, not a tag's.
	Pipeline *string `json:"pipeline,omitempty" validate:"omitempty,max=63"`

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

	// Domain is the hostname a public service is served on (RFC 017
	// §2.3.1).
	//
	// It exists because not every platform can hand back an HTTPS endpoint
	// unprompted. Cloud Run serves `*.run.app` with a Google-managed
	// certificate; AWS has no equivalent, and ACM will not issue a
	// certificate for an ALB's own `*.elb.amazonaws.com` name. Without a
	// hostname to name, `public: true` on AWS could only have been
	// delivered as a plain HTTP listener, which §2.3 refuses.
	//
	// Bounded at 253 characters, the maximum length of a DNS name.
	Domain string `json:"domain,omitempty" validate:"omitempty,hostname_rfc1123,max=253"`
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

// ErrDomainWithoutPublic indicates a `domain` on a service nothing outside
// can reach (RFC 017 §2.3.1).
//
// Refused rather than ignored, on the RFC 011 §2.1 principle: a property
// the provider does not act on is a request the user made and will not
// get, and it must surface at Validate time rather than as a hostname that
// resolves nowhere.
var ErrDomainWithoutPublic = errors.New("container_service: `domain` requires `public: true`")

// ValidateIngress applies the cross-field rules between `public` and
// `domain` that no struct tag can express (RFC 017 §2.3.1).
//
// Only the half that holds on every provider lives here. "A public service
// needs a domain" is an AWS limitation, not a property of the schema —
// Cloud Run is public without one — so it belongs in the AWS provider,
// where RFC 012 §1.3's rule applies: a request a provider cannot express
// is refused by that provider, and by name.
func (p Properties) ValidateIngress() error {
	if p.Domain != "" && !p.EffectivePublic() {
		return fmt.Errorf("%w: %q", ErrDomainWithoutPublic, p.Domain)
	}
	return nil
}

// Image source errors (RFC 018 §3).
var (
	// ErrNoImageSource indicates a service that names neither an image nor
	// a pipeline, and so describes no code to run.
	ErrNoImageSource = errors.New("container_service: exactly one of `image` or `pipeline` is required, and neither is set")

	// ErrAmbiguousImageSource indicates both are set. Refused rather than
	// resolved by precedence: a rule saying which wins is a rule every
	// reader has to know before they can tell what a Specification deploys
	// (RFC 018 §3).
	ErrAmbiguousImageSource = errors.New("container_service: `image` and `pipeline` are mutually exclusive")

	// ErrEmptyPipelineReference indicates `"pipeline": ""` — a reference
	// the user wrote and left blank, which is a different mistake from
	// omitting it.
	ErrEmptyPipelineReference = errors.New("container_service: `pipeline` is present but empty")
)

// PipelineRef returns the build_pipeline this service consumes, and
// whether it names one at all.
func (p Properties) PipelineRef() (string, bool) {
	if p.Pipeline == nil {
		return "", false
	}
	return *p.Pipeline, true
}

// ValidateSource enforces RFC 018 §3's exactly-one-of rule between `image`
// and `pipeline`.
//
// It lives beside ValidateIngress rather than in a struct tag for the same
// reason: `excluded_with` could express the exclusion but not the
// requirement that one of them be present, and splitting one rule across
// two mechanisms is how half of it gets forgotten.
func (p Properties) ValidateSource() error {
	ref, named := p.PipelineRef()
	switch {
	case named && ref == "":
		return ErrEmptyPipelineReference
	case p.Image != "" && named:
		return fmt.Errorf("%w: image %q and pipeline %q", ErrAmbiguousImageSource, p.Image, ref)
	case p.Image == "" && !named:
		return ErrNoImageSource
	}
	return nil
}

// ValidateImageSource is the single check every provider runs over where a
// service's code comes from: exactly one source is named, and a named
// *image* satisfies RFC 017 §2.4's pinning and allowlist rules.
//
// An image produced by a pipeline is exempt from both, and deliberately so
// (RFC 018 §2.4). Its digest does not exist until the build runs, so no
// Specification could pin it; the provenance guarantee is moved rather than
// dropped — the revision is resolved to a commit at plan time and it is a
// commit the user approves. The allowlist is likewise satisfied implicitly:
// the image comes from the registry CloudSDD created in the user's own
// account.
//
// It exists so the three providers call one function rather than three
// copies of the same conditional (RFC 011 §1.1H).
func (p Properties) ValidateImageSource(allowedRegistries []string) error {
	if err := p.ValidateSource(); err != nil {
		return err
	}
	if _, viaPipeline := p.PipelineRef(); viaPipeline {
		return nil
	}
	return ValidateImage(p.Image, allowedRegistries)
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
