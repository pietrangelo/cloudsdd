// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package pipeline

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Revision resolution (RFC 018 §2.4).
//
// A branch is allowed in a Specification where a `latest` tag is not, and
// the difference is where it gets resolved. `latest` is resolved by the
// registry at pull time, after review, and can change again afterwards. A
// branch is resolved *here*, before review, and what the user approves is a
// commit.
//
// The resolved SHA is then the name of the built image: the build tags with
// it, and the container_service consuming the pipeline asks for that tag.
// A registry with immutable tags makes that as strong as a digest, without
// needing to read anything back out of a build that has not run yet.

// commitSHA matches a full 40-character Git object name. Abbreviated SHAs
// are deliberately not accepted: an abbreviation is a prefix, prefixes
// collide, and this is the value that decides what code runs.
var commitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// IsCommitSHA reports whether revision is already a full commit SHA and so
// needs no resolution.
func IsCommitSHA(revision string) bool { return commitSHA.MatchString(revision) }

// Revision resolution errors.
var (
	// ErrRevisionNotFound indicates a branch or tag the repository does
	// not have.
	ErrRevisionNotFound = errors.New("build_pipeline: `source.revision` does not exist in the repository")

	// ErrRepositoryUnreachable indicates the repository could not be
	// queried at all.
	//
	// It is an error rather than a fallback to "resolve it at build time".
	// A build whose commit was never shown is a build nobody approved,
	// which is the whole thing §2.4 exists to prevent.
	ErrRepositoryUnreachable = errors.New("build_pipeline: `source.repository` could not be reached")

	// ErrAmbiguousRevision indicates a name that is both a branch and a
	// tag, pointing at different commits. Refused rather than resolved by
	// precedence: git's own precedence rules are not something a reviewer
	// should have to know to tell what will be deployed.
	ErrAmbiguousRevision = errors.New("build_pipeline: `source.revision` is both a branch and a tag")
)

// RevisionResolver turns a branch, tag or SHA into the commit it names.
type RevisionResolver interface {
	// Resolve returns the full commit SHA for revision in repository.
	Resolve(ctx context.Context, repository, revision string) (string, error)
}

// httpResolver resolves through Git's smart HTTP transport.
//
// It speaks the protocol directly rather than shelling out to `git` or
// taking a Git library as a dependency. Ref discovery is one unauthenticated
// GET returning a pkt-line list of every ref and its object name, which is
// exactly the question being asked — and CLAUDE.md's "zero unnecessary heavy
// external dependencies" makes a whole Git implementation a steep price for
// one lookup. It also keeps this host-agnostic: a forge-specific REST API
// would work on github.com and nowhere else.
type httpResolver struct {
	client *http.Client
}

// NewRevisionResolver returns the resolver the Engine uses.
//
// The timeout is short and deliberate: this runs during plan, in front of a
// user waiting to see what will be deployed, and a repository that does not
// answer promptly should say so rather than hang.
func NewRevisionResolver() RevisionResolver {
	return httpResolver{client: &http.Client{Timeout: 15 * time.Second}}
}

var _ RevisionResolver = httpResolver{}

// Resolve implements RevisionResolver.
func (r httpResolver) Resolve(ctx context.Context, repository, revision string) (string, error) {
	// A revision that is already a commit needs no lookup, which also
	// means a Specification pinned to a SHA plans without network access.
	if IsCommitSHA(revision) {
		return revision, nil
	}

	refs, err := r.discover(ctx, repository)
	if err != nil {
		return "", err
	}

	// A tag and a branch may share a name. Both are looked up so the
	// collision is visible rather than decided silently.
	branch, hasBranch := refs["refs/heads/"+revision]
	tag, hasTag := refs["refs/tags/"+revision]
	// An annotated tag's peeled entry is the commit it points at; the tag
	// object itself is not one, and building it would check out nothing.
	if peeled, ok := refs["refs/tags/"+revision+"^{}"]; ok {
		tag, hasTag = peeled, true
	}

	switch {
	case hasBranch && hasTag && branch != tag:
		return "", fmt.Errorf("%w: %q is branch %s and tag %s", ErrAmbiguousRevision, revision, branch, tag)
	case hasBranch:
		return branch, nil
	case hasTag:
		return tag, nil
	}
	return "", fmt.Errorf("%w: %q", ErrRevisionNotFound, revision)
}

// maxRefsResponse bounds what a repository can make this read. A remote
// that is hostile, or merely enormous, must not be able to exhaust memory
// during a plan.
const maxRefsResponse = 8 << 20 // 8 MiB

// discover performs Git's ref advertisement and returns ref name -> object
// name.
func (r httpResolver) discover(ctx context.Context, repository string) (map[string]string, error) {
	endpoint := strings.TrimSuffix(repository, "/") + "/info/refs?service=git-upload-pack"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRepositoryUnreachable, err)
	}
	// Protocol v0 explicitly. v2 answers this same endpoint with a
	// capability list and *no refs at all* — under v2 the ref list is a
	// separate `ls-refs` command sent by POST — so asking for v2 here
	// would parse cleanly and find nothing, which is the worst shape a
	// failure can take. Servers default to v0 when unasked; saying so
	// makes the dependency visible rather than inherited.
	req.Header.Set("Git-Protocol", "version=0")

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRepositoryUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// 401 and 404 are the same answer from a private repository: it
		// exists and will not talk to an anonymous client. RFC 018 §2.6
		// builds from public repositories only, so both mean the same
		// thing here, and saying so is more useful than the status code.
		return nil, fmt.Errorf("%w: %s answered %s (this RFC builds from public repositories only)",
			ErrRepositoryUnreachable, repository, resp.Status)
	}

	return parseRefAdvertisement(io.LimitReader(resp.Body, maxRefsResponse))
}

// parseRefAdvertisement reads the pkt-line stream Git answers ref discovery
// with, returning ref name -> object name.
//
// Each line is a four-hex-digit length covering itself, then the payload.
// "0000" is a flush packet. The advertisement's own header lines ("#
// service=...") and capability suffixes after a NUL are skipped.
func parseRefAdvertisement(body io.Reader) (map[string]string, error) {
	reader := bufio.NewReader(body)
	refs := map[string]string{}

	for {
		var length [4]byte
		if _, err := io.ReadFull(reader, length[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return nil, fmt.Errorf("%w: %v", ErrRepositoryUnreachable, err)
		}

		size, err := strconv.ParseUint(string(length[:]), 16, 32)
		if err != nil {
			return nil, fmt.Errorf("%w: malformed packet length %q", ErrRepositoryUnreachable, length)
		}
		// A flush packet ends a section; the stream may continue after it.
		if size == 0 {
			continue
		}
		if size < 4 {
			return nil, fmt.Errorf("%w: packet length %d is shorter than its own header", ErrRepositoryUnreachable, size)
		}

		payload := make([]byte, size-4)
		if _, err := io.ReadFull(reader, payload); err != nil {
			return nil, fmt.Errorf("%w: truncated packet: %v", ErrRepositoryUnreachable, err)
		}

		line := strings.TrimRight(string(payload), "\n")
		// Capabilities ride on the first ref line, after a NUL.
		if nul := strings.IndexByte(line, 0); nul >= 0 {
			line = line[:nul]
		}
		if strings.HasPrefix(line, "#") {
			continue
		}

		object, name, ok := strings.Cut(line, " ")
		if !ok || !commitSHA.MatchString(object) {
			continue
		}
		refs[name] = object
	}

	if len(refs) == 0 {
		return nil, fmt.Errorf("%w: the repository advertised no refs", ErrRepositoryUnreachable)
	}
	return refs, nil
}
