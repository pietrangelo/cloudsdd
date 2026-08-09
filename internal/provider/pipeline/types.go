// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

// Package pipeline provides the cloud-agnostic definitions and pure functions
// required for the build_pipeline resource type (RFC 018).
//
// The shape lives here rather than being redeclared per provider for the
// reason RFC 011 §1.1H recorded and RFC 013 and RFC 017 followed: three
// copies of a schema drift, and a drifted schema means a Specification that
// is valid on one cloud and silently different on another. Each provider
// still decodes through its own decode.Decoder, so error prefixes stay
// where they belong; only the shape is shared.
package pipeline

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Runtime identifies the supported language runtimes for a build_pipeline.
//
// The enum is deliberately small and holds only what all three build
// services can produce (RFC 018 §2.2). A runtime that worked on one cloud
// would make "agnostic" resolution silently provider-specific, which is
// the coupling RFC 013 §7.2 rejected for compute_instance images.
type Runtime string

const (
	RuntimeGo     Runtime = "go"
	RuntimeNode   Runtime = "node"
	RuntimePython Runtime = "python"
	RuntimeJava   Runtime = "java"
)

// Stack describes the runtime environment for the built image. It is what
// the user names instead of writing a Dockerfile: CloudSDD generates one
// (RFC 018 §2.2), which is what makes the mandate's "state of the art
// without asking" reach the image's contents and not just its
// surroundings.
type Stack struct {
	Runtime Runtime `json:"runtime" validate:"required,oneof=go node python java"`

	// Version is the runtime version, e.g. "1.22". Its accepted forms are
	// per-runtime and are checked by the Dockerfile generator, which is the
	// component that has to turn one into a base image.
	Version string `json:"version" validate:"required,max=32"`
}

// Source names the code to build.
//
// There is deliberately no credential field. A private repository needs a
// long-lived secret, and the only place a Specification could carry one is
// a property — which internal/provider/decode's denylist rejects by name,
// correctly. So this RFC builds from public repositories only, and private
// sources wait for the RFC that can pair them with a secrets story (RFC
// 018 §2.6).
type Source struct {
	// Repository is the HTTPS URL of a public Git repository. Constrained
	// by Validate rather than by a tag alone: `url` accepts schemes and
	// userinfo that this must refuse.
	Repository string `json:"repository" validate:"required,max=512"`

	// Revision is a branch, a tag or a commit SHA. It is resolved to a
	// commit SHA at plan time and shown, so what the user approves is a
	// commit rather than "whatever is newest" (RFC 018 §2.4).
	Revision string `json:"revision" validate:"required,max=255"`
}

// Retention bounds (RFC 018 §2.5). Five is the default the RFC sets. One
// is the floor because a retention policy that keeps nothing is a registry
// that holds nothing, and the ceiling is the usual reason every other
// bound in this codebase exists: a mistranslated prompt should not be able
// to request an unbounded storage bill.
const (
	DefaultRetain = 5
	MaxRetain     = 100
)

// MaxPorts caps the declared port list, on the same reasoning.
const MaxPorts = 10

// GeneratedDockerfileName is the file each build service writes the
// generated Dockerfile to before building it.
//
// Deliberately not "Dockerfile": a repository that has one of its own is
// not overwritten, and the difference between what the repository builds
// and what CloudSDD builds stays visible. All three clouds agree on the
// name, so it is stated once (RFC 019 §2.2).
const GeneratedDockerfileName = "Dockerfile.cloudsdd"

// BuildPipelineProperties represents the Properties of a Resource with
// Type "build_pipeline" on any provider (RFC 018 §2.1).
type BuildPipelineProperties struct {
	Source Source `json:"source" validate:"required"`
	Stack  Stack  `json:"stack" validate:"required"`

	// ImageName is the repository name within the registry CloudSDD
	// creates. Constrained by Validate: the OCI grammar is not a tag.
	ImageName string `json:"image_name" validate:"required,max=200"`

	// Ports are the ports the image declares (EXPOSE), and the set a
	// container_service may choose its single ingress port from (RFC 018
	// §2.3). This is not a list of ports to expose to the internet: only
	// the service's own `port` is routed, and the rest are reachable from
	// the scope's network, which is what makes a metrics or admin port
	// expressible without publishing it.
	Ports []int `json:"ports,omitempty" validate:"omitempty,max=10,unique,dive,min=1,max=65535"`

	// Retain is how many images the registry keeps. A pointer so that
	// absence selects DefaultRetain rather than the zero value, the
	// tri-state pattern RFC 011 §2.5 established.
	Retain *int `json:"retain,omitempty" validate:"omitempty,min=1,max=100"`
}

// EffectiveRetain returns Retain, or DefaultRetain when absent.
func (p BuildPipelineProperties) EffectiveRetain() int {
	if p.Retain == nil {
		return DefaultRetain
	}
	return *p.Retain
}

// DeclaresPort reports whether port is one the image exposes. It is what
// the Engine asks before letting a container_service route to it (RFC 018
// §2.3).
//
// An empty Ports list declares nothing, so nothing agrees with it. That is
// deliberate: a service pointed at a pipeline that names no ports is a
// deployment that starts and answers nothing, and the Engine should say so
// rather than assume the image happens to listen where the service hopes.
func (p BuildPipelineProperties) DeclaresPort(port int) bool {
	for _, declared := range p.Ports {
		if declared == port {
			return true
		}
	}
	return false
}

// Source and image-name errors (RFC 018 §2.4, §2.6).
var (
	// ErrRepositoryScheme indicates a repository URL that is not HTTPS.
	// Refused rather than upgraded: `git://` and `http://` carry the clone
	// unauthenticated and unencrypted, so what gets built is whatever the
	// network returned.
	ErrRepositoryScheme = errors.New("build_pipeline: `source.repository` must be an https:// URL")

	// ErrRepositoryCredential indicates userinfo in the URL —
	// https://user:token@host/repo. This is how a credential ends up in a
	// reviewed document despite there being no property to put one in, so
	// it is refused by shape (RFC 018 §2.6).
	ErrRepositoryCredential = errors.New("build_pipeline: `source.repository` must not carry credentials")

	// ErrRepositoryMalformed covers a URL no clone would accept.
	ErrRepositoryMalformed = errors.New("build_pipeline: `source.repository` is malformed")

	// ErrRevisionMalformed indicates a revision outside the accepted
	// charset. The revision reaches a build service as an argument, so the
	// grammar is narrow on purpose.
	ErrRevisionMalformed = errors.New("build_pipeline: `source.revision` is malformed")

	// ErrImageNameMalformed indicates a name outside the OCI repository
	// grammar, which no registry would accept.
	ErrImageNameMalformed = errors.New("build_pipeline: `image_name` is malformed")
)

// ErrNotResolved indicates a container_service whose `pipeline` reference
// reached a provider without the Engine's answer (RFC 018 §2.4.1).
//
// It is refused rather than guessed at. Guessing would mean inventing a
// repository name and a tag, and the two plausible inventions — the
// pipeline's resource id, or `latest` — are respectively wrong and the one
// thing RFC 017 §2.4 refuses outright.
//
// One sentinel for three clouds: each provider wraps it, so the prefix
// stays local while errors.Is answers the same question everywhere.
var ErrNotResolved = errors.New("build_pipeline: `pipeline` reached the provider unresolved")

// DecodeAndValidate turns a Resource's raw Properties into a validated
// BuildPipelineProperties, through the caller's own strict decoder.
//
// The three steps are the ones every provider performed identically before
// RFC 019 §2.2 collapsed them: decode, apply the shared rules, and generate
// the Dockerfile — the last of these purely to refuse an unbuildable stack
// while the user is still reading a plan, rather than by a build that has
// already been provisioned and started.
//
// decode is a plain func, satisfied by decode.Decoder.Properties, so that
// this package keeps the zero internal imports RFC 019 §2.1 protects.
// prefix names the provider, and labels only the failures this function
// owns: a decoder that refuses the properties has already named itself, and
// relabelling it would say so twice.
func DecodeAndValidate(
	props map[string]any,
	decode func(map[string]any, any) error,
	prefix string,
) (*BuildPipelineProperties, error) {
	var p BuildPipelineProperties
	if err := decode(props, &p); err != nil {
		return nil, err
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", prefix, err)
	}
	if _, err := NewDockerfileGenerator().Generate(BuildSpec{Stack: p.Stack, Ports: p.Ports}); err != nil {
		return nil, fmt.Errorf("%s: %w", prefix, err)
	}
	return &p, nil
}

// revisionPattern is what a Git branch, tag or SHA may look like here.
//
// Narrower than git's own refname rules, and intentionally so: this string
// is handed to a managed build service as an argument, and a value that
// parses two ways is a value whose meaning is decided by whichever cloud
// receives it — the same reasoning RFC 017 §2.4 applied to image
// references.
var revisionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,254}$`)

// imageNamePattern is the OCI repository-name grammar: lowercase
// alphanumeric components, separated by one of `.`, `_`, `-`, in
// slash-delimited path segments.
var imageNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$`)

// Validate applies the cross-field and grammar rules no struct tag can
// express.
//
// Every provider calls it after decoding, for the reason RFC 011 §1.1H
// gave: a check that lives in one provider is a check the next provider
// forgets.
func (p BuildPipelineProperties) Validate() error {
	if err := p.Source.Validate(); err != nil {
		return err
	}
	if !imageNamePattern.MatchString(p.ImageName) {
		return fmt.Errorf("%w: %q", ErrImageNameMalformed, p.ImageName)
	}
	// A leading component that looks like a host is refused, by the same
	// test container.ParseImage uses to tell a registry from a Docker Hub
	// namespace. It is a legal OCI repository name, so this is a rule and
	// not a grammar: the registry is the one CloudSDD creates, and a name
	// that appears to choose a different one describes something the user
	// is not going to get — most likely a full image reference pasted
	// where a repository name belongs.
	if head, _, ok := strings.Cut(p.ImageName, "/"); ok {
		if strings.ContainsAny(head, ".:") || head == "localhost" {
			return fmt.Errorf("%w: %q names a registry; `image_name` is a repository within the one CloudSDD creates",
				ErrImageNameMalformed, p.ImageName)
		}
	}
	return nil
}

// Validate checks the repository URL and the revision.
func (s Source) Validate() error {
	u, err := url.Parse(s.Repository)
	if err != nil {
		return fmt.Errorf("%w: %q: %v", ErrRepositoryMalformed, s.Repository, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("%w: %q uses %q", ErrRepositoryScheme, s.Repository, u.Scheme)
	}
	if u.User != nil {
		// The URL is not echoed: it is the half of the value that is the
		// secret. Reporting the host is enough to find the offending entry.
		return fmt.Errorf("%w: host %q", ErrRepositoryCredential, u.Host)
	}
	if u.Host == "" || strings.Trim(u.Path, "/") == "" {
		return fmt.Errorf("%w: %q names no repository", ErrRepositoryMalformed, s.Repository)
	}
	if !revisionPattern.MatchString(s.Revision) || strings.Contains(s.Revision, "..") {
		return fmt.Errorf("%w: %q", ErrRevisionMalformed, s.Revision)
	}
	return nil
}
