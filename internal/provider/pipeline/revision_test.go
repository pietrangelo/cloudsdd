// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package pipeline

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	mainSHA = "1c9e0aa5a5e14b6d34a3f9c1d0d0b1b9f0a1c2d3"
	tagSHA  = "9f8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c"
)

// pktLine encodes one line of Git's pkt-line framing.
func pktLine(payload string) string {
	return fmt.Sprintf("%04x%s", len(payload)+4, payload)
}

// advertisement builds a ref advertisement of the shape a Git server sends.
func advertisement(refs ...string) string {
	var b strings.Builder
	b.WriteString(pktLine("# service=git-upload-pack\n"))
	b.WriteString("0000")
	for i, ref := range refs {
		if i == 0 {
			// Capabilities ride on the first ref line, after a NUL.
			b.WriteString(pktLine(ref + "\x00multi_ack symref=HEAD:refs/heads/main\n"))
			continue
		}
		b.WriteString(pktLine(ref + "\n"))
	}
	b.WriteString("0000")
	return b.String()
}

// refServer serves one advertisement and records what was asked for.
func refServer(t *testing.T, status int, body string) (*httptest.Server, *string) {
	t.Helper()
	var asked string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server, &asked
}

// TestResolveRevision covers RFC 018 §2.4: a branch or tag becomes the
// commit it names, before the user approves anything.
func TestResolveRevision(t *testing.T) {
	body := advertisement(
		mainSHA+" refs/heads/main",
		tagSHA+" refs/tags/v1.4.0",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa refs/heads/release/2026-08",
	)

	tests := []struct {
		name     string
		revision string
		want     string
		wantErr  error
	}{
		{name: "a branch", revision: "main", want: mainSHA},
		{name: "a tag", revision: "v1.4.0", want: tagSHA},
		{name: "a namespaced branch", revision: "release/2026-08", want: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{
			// A ref the repository does not have is refused, never passed
			// through for the build to fail on later.
			name:     "a branch that does not exist",
			revision: "does-not-exist",
			wantErr:  ErrRevisionNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, asked := refServer(t, http.StatusOK, body)
			resolver := httpResolver{client: server.Client()}

			got, err := resolver.Resolve(context.Background(), server.URL+"/acme/api", tt.revision)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Resolve() = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve() = %v, want nil", err)
			}
			if got != tt.want {
				t.Errorf("Resolve() = %q, want %q", got, tt.want)
			}
			if want := "/acme/api/info/refs?service=git-upload-pack"; *asked != want {
				t.Errorf("asked for %q, want %q", *asked, want)
			}
		})
	}
}

// TestResolveRevisionShortCircuitsASHA: a Specification already pinned to a
// commit must plan without touching the network.
func TestResolveRevisionShortCircuitsASHA(t *testing.T) {
	resolver := httpResolver{client: &http.Client{Transport: refusingTransport{t}}}

	got, err := resolver.Resolve(context.Background(), "https://github.com/acme/api", mainSHA)
	if err != nil {
		t.Fatalf("Resolve() = %v, want nil", err)
	}
	if got != mainSHA {
		t.Errorf("Resolve() = %q, want %q", got, mainSHA)
	}
}

// refusingTransport fails the test if any request is made.
type refusingTransport struct{ t *testing.T }

func (r refusingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	r.t.Error("Resolve() contacted the network for a revision that is already a commit")
	return nil, errors.New("no request should have been made")
}

// TestResolveAnnotatedTag: an annotated tag object is not a commit, and
// building it would check out nothing. The peeled entry is what counts.
func TestResolveAnnotatedTag(t *testing.T) {
	body := advertisement(
		mainSHA+" refs/heads/main",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb refs/tags/v2.0.0",
		tagSHA+" refs/tags/v2.0.0^{}",
	)
	server, _ := refServer(t, http.StatusOK, body)
	resolver := httpResolver{client: server.Client()}

	got, err := resolver.Resolve(context.Background(), server.URL+"/acme/api", "v2.0.0")
	if err != nil {
		t.Fatalf("Resolve() = %v, want nil", err)
	}
	if got != tagSHA {
		t.Errorf("Resolve() = %q, want the peeled commit %q", got, tagSHA)
	}
}

// TestResolveAmbiguousRevision: a name that is both a branch and a tag is
// refused rather than resolved by git's precedence rules, which are not
// something a reviewer should need to know to tell what will be deployed.
func TestResolveAmbiguousRevision(t *testing.T) {
	t.Run("branch and tag disagree", func(t *testing.T) {
		body := advertisement(
			mainSHA+" refs/heads/release",
			tagSHA+" refs/tags/release",
		)
		server, _ := refServer(t, http.StatusOK, body)
		resolver := httpResolver{client: server.Client()}

		_, err := resolver.Resolve(context.Background(), server.URL+"/acme/api", "release")
		if !errors.Is(err, ErrAmbiguousRevision) {
			t.Fatalf("Resolve() = %v, want %v", err, ErrAmbiguousRevision)
		}
	})

	t.Run("branch and tag agree", func(t *testing.T) {
		// Both names, one commit: there is nothing to be ambiguous about.
		body := advertisement(
			mainSHA+" refs/heads/release",
			mainSHA+" refs/tags/release",
		)
		server, _ := refServer(t, http.StatusOK, body)
		resolver := httpResolver{client: server.Client()}

		got, err := resolver.Resolve(context.Background(), server.URL+"/acme/api", "release")
		if err != nil {
			t.Fatalf("Resolve() = %v, want nil", err)
		}
		if got != mainSHA {
			t.Errorf("Resolve() = %q, want %q", got, mainSHA)
		}
	})
}

// TestResolveUnreachableRepository covers the failures that must not become
// a fallback. A build whose commit was never shown is a build nobody
// approved.
func TestResolveUnreachableRepository(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{
			// A private repository answers 401 or 404 to an anonymous
			// client; both mean the same thing under §2.6.
			name:   "unauthorized",
			status: http.StatusUnauthorized,
			body:   "",
		},
		{name: "not found", status: http.StatusNotFound, body: ""},
		{name: "server error", status: http.StatusInternalServerError, body: ""},
		{
			name:   "an empty advertisement",
			status: http.StatusOK,
			body:   "0000",
		},
		{
			name:   "a malformed packet length",
			status: http.StatusOK,
			body:   "zzzz" + mainSHA + " refs/heads/main\n",
		},
		{
			name:   "a packet shorter than its own header",
			status: http.StatusOK,
			body:   "0002xx",
		},
		{
			name:   "a truncated packet",
			status: http.StatusOK,
			body:   pktLine("# service=git-upload-pack\n") + "0100short",
		},
		{
			name:   "an advertisement with no refs",
			status: http.StatusOK,
			body:   advertisement(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, _ := refServer(t, tt.status, tt.body)
			resolver := httpResolver{client: server.Client()}

			_, err := resolver.Resolve(context.Background(), server.URL+"/acme/api", "main")
			if !errors.Is(err, ErrRepositoryUnreachable) {
				t.Fatalf("Resolve() = %v, want %v", err, ErrRepositoryUnreachable)
			}
		})
	}
}

// TestResolveRejectsAHostileAdvertisement: the response is bounded, so a
// remote that is hostile or merely enormous cannot exhaust memory during a
// plan.
func TestResolveRejectsAHostileAdvertisement(t *testing.T) {
	var b strings.Builder
	b.WriteString(pktLine("# service=git-upload-pack\n"))
	// Well past the 8 MiB ceiling, in valid packets.
	for i := 0; b.Len() < maxRefsResponse+(1<<20); i++ {
		b.WriteString(pktLine(fmt.Sprintf("%s refs/heads/branch-%06d\n", mainSHA, i)))
	}
	server, _ := refServer(t, http.StatusOK, b.String())
	resolver := httpResolver{client: server.Client()}

	// It must terminate, and it must not have read the whole body. What it
	// returns is not the point: that it stops is.
	if _, err := resolver.Resolve(context.Background(), server.URL+"/acme/api", "branch-000000"); err != nil {
		t.Logf("Resolve() = %v (acceptable: the response was truncated at the ceiling)", err)
	}
}

// TestIsCommitSHA: an abbreviation is a prefix, prefixes collide, and this
// is the value that decides what code runs.
func TestIsCommitSHA(t *testing.T) {
	tests := []struct {
		name     string
		revision string
		want     bool
	}{
		{name: "a full sha", revision: mainSHA, want: true},
		{name: "an abbreviated sha", revision: mainSHA[:12]},
		{name: "uppercase", revision: strings.ToUpper(mainSHA)},
		{name: "a branch", revision: "main"},
		{name: "a sha with a suffix", revision: mainSHA + "0"},
		{name: "empty", revision: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsCommitSHA(tt.revision); got != tt.want {
				t.Errorf("IsCommitSHA(%q) = %v, want %v", tt.revision, got, tt.want)
			}
		})
	}
}

// TestResolveRequestsProtocolV0 pins the one thing a fixture cannot catch
// by shape.
//
// Protocol v2 answers /info/refs with a capability list and no refs at all
// — under v2 the ref list is a separate `ls-refs` command sent by POST — so
// asking for v2 parses cleanly and finds nothing. A test built from a v0
// fixture passes either way; only the header says which protocol was
// actually requested.
func TestResolveRequestsProtocolV0(t *testing.T) {
	var got string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Git-Protocol")
		_, _ = w.Write([]byte(advertisement(mainSHA + " refs/heads/main")))
	}))
	t.Cleanup(server.Close)

	resolver := httpResolver{client: server.Client()}
	if _, err := resolver.Resolve(context.Background(), server.URL+"/acme/api", "main"); err != nil {
		t.Fatalf("Resolve() = %v, want nil", err)
	}
	if got == "version=2" {
		t.Fatal("requested protocol v2, whose /info/refs advertises no refs")
	}
	if got != "version=0" {
		t.Errorf("Git-Protocol = %q, want %q", got, "version=0")
	}
}

// TestNewRevisionResolverIsBounded: the default resolver runs during plan,
// in front of a user waiting to see what will be deployed, so a repository
// that does not answer must fail rather than hang.
func TestNewRevisionResolverIsBounded(t *testing.T) {
	resolver, ok := NewRevisionResolver().(httpResolver)
	if !ok {
		t.Fatalf("NewRevisionResolver() = %T, want httpResolver", NewRevisionResolver())
	}
	if resolver.client.Timeout <= 0 {
		t.Error("the default resolver has no timeout; an unresponsive repository would hang a plan")
	}

	// And it resolves a SHA without a network, which is the path a
	// fully-pinned Specification takes.
	got, err := resolver.Resolve(context.Background(), "https://github.com/acme/api", mainSHA)
	if err != nil || got != mainSHA {
		t.Fatalf("Resolve() = (%q, %v), want (%q, nil)", got, err, mainSHA)
	}
}

// TestResolveRefusesAMalformedRepositoryURL: a URL net/http cannot even
// build a request for is unreachable, not a panic.
func TestResolveRefusesAMalformedRepositoryURL(t *testing.T) {
	resolver := httpResolver{client: &http.Client{}}

	for _, repository := range []string{"://not a url", "https://exa mple.com/acme/api"} {
		if _, err := resolver.Resolve(context.Background(), repository, "main"); !errors.Is(err, ErrRepositoryUnreachable) {
			t.Errorf("Resolve(%q) = %v, want %v", repository, err, ErrRepositoryUnreachable)
		}
	}
}

// TestResolveIgnoresNonRefLines: the advertisement carries lines that are
// not refs, and a parser that took them for refs would resolve a name to
// something that is not a commit.
func TestResolveIgnoresNonRefLines(t *testing.T) {
	var b strings.Builder
	b.WriteString(pktLine("# service=git-upload-pack\n"))
	b.WriteString("0000")
	b.WriteString(pktLine(mainSHA + " refs/heads/main\x00multi_ack\n"))
	b.WriteString(pktLine("shallow " + tagSHA + "\n"))
	b.WriteString(pktLine("not-a-sha refs/heads/broken\n"))
	b.WriteString(pktLine("nospaceonthisline\n"))
	b.WriteString("0000")

	server, _ := refServer(t, http.StatusOK, b.String())
	resolver := httpResolver{client: server.Client()}

	got, err := resolver.Resolve(context.Background(), server.URL+"/acme/api", "main")
	if err != nil {
		t.Fatalf("Resolve() = %v, want nil", err)
	}
	if got != mainSHA {
		t.Errorf("Resolve() = %q, want %q", got, mainSHA)
	}
	if _, err := resolver.Resolve(context.Background(), server.URL+"/acme/api", "broken"); !errors.Is(err, ErrRevisionNotFound) {
		t.Errorf("a line whose object is not a SHA was taken for a ref: %v", err)
	}
}
