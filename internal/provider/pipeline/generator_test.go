// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package pipeline

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// -update rewrites the golden files. It exists so that a deliberate change
// to a generated Dockerfile is a diff in review (RFC 018 §5.1), which is
// the whole reason these are golden files and not assertions.
var update = flag.Bool("update", false, "rewrite the Dockerfile golden files")

// TestDockerfileGeneration compares the generated Dockerfile for each
// runtime against its golden file.
func TestDockerfileGeneration(t *testing.T) {
	tests := []struct {
		name   string
		golden string
		spec   BuildSpec
	}{
		{
			name:   "go",
			golden: "go.Dockerfile",
			spec:   BuildSpec{Stack: Stack{Runtime: RuntimeGo, Version: "1.22"}, Ports: []int{8080}},
		},
		{
			name:   "node",
			golden: "node.Dockerfile",
			spec:   BuildSpec{Stack: Stack{Runtime: RuntimeNode, Version: "22"}, Ports: []int{3000}},
		},
		{
			name:   "python",
			golden: "python.Dockerfile",
			spec:   BuildSpec{Stack: Stack{Runtime: RuntimePython, Version: "3.12"}, Ports: []int{8000}},
		},
		{
			name:   "java",
			golden: "java.Dockerfile",
			spec:   BuildSpec{Stack: Stack{Runtime: RuntimeJava, Version: "21"}, Ports: []int{8080}},
		},
		{
			// The metrics-port case from RFC 018 §2.3: the image declares
			// more than the service routes to.
			name:   "several declared ports",
			golden: "go-multiport.Dockerfile",
			spec:   BuildSpec{Stack: Stack{Runtime: RuntimeGo, Version: "1.24"}, Ports: []int{8080, 9090}},
		},
		{
			name:   "no declared ports",
			golden: "go-noports.Dockerfile",
			spec:   BuildSpec{Stack: Stack{Runtime: RuntimeGo, Version: "1.23"}},
		},
	}

	gen := NewDockerfileGenerator()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := gen.Generate(tt.spec)
			if err != nil {
				t.Fatalf("Generate() = %v, want nil", err)
			}

			path := filepath.Join("testdata", tt.golden)
			if *update {
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatalf("failed to write the golden file: %v", err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("failed to read the golden file (run with -update to create it): %v", err)
			}
			if got != string(want) {
				t.Errorf("generated Dockerfile differs from %s:\n--- got ---\n%s\n--- want ---\n%s",
					tt.golden, got, want)
			}
		})
	}
}

// fromPattern matches a FROM instruction and captures its image reference.
var fromPattern = regexp.MustCompile(`(?m)^FROM\s+(\S+)`)

// TestDockerfileInvariants asserts the four properties RFC 018 §2.2 makes
// unconditional, for every runtime and every pinned version.
//
// It is table-driven over the pin table rather than over a fixed list, so a
// version added without these properties fails here rather than shipping.
// The golden files show *what* is generated; this shows what may never stop
// being true of it.
func TestDockerfileInvariants(t *testing.T) {
	gen := NewDockerfileGenerator()

	for _, runtime := range SupportedRuntimes() {
		for _, version := range SupportedVersions(runtime) {
			t.Run(string(runtime)+" "+version, func(t *testing.T) {
				got, err := gen.Generate(BuildSpec{
					Stack: Stack{Runtime: runtime, Version: version},
					Ports: []int{8080},
				})
				if err != nil {
					t.Fatalf("Generate() = %v, want nil", err)
				}

				// (2) multi-stage: a build stage the final image discards.
				froms := fromPattern.FindAllStringSubmatch(got, -1)
				if len(froms) < 2 {
					t.Errorf("Dockerfile has %d FROM instructions, want a multi-stage build", len(froms))
				}
				if !strings.Contains(got, "AS build") {
					t.Error("Dockerfile declares no named build stage")
				}

				// (3) every base image is pinned by digest.
				for _, from := range froms {
					ref := from[1]
					if ref == "build" || strings.HasPrefix(ref, "--") {
						continue
					}
					if !strings.Contains(ref, "@sha256:") {
						t.Errorf("FROM %q is not pinned to a digest", ref)
					}
				}

				// (1) a non-root user, switched to before the entrypoint.
				user := strings.Index(got, "\nUSER ")
				if user < 0 {
					t.Fatal("Dockerfile never switches away from root")
				}
				if strings.Contains(got, "\nUSER root") || strings.Contains(got, "\nUSER 0") {
					t.Error("Dockerfile switches to root")
				}
				for _, entry := range []string{"\nENTRYPOINT ", "\nCMD "} {
					if at := strings.Index(got, entry); at >= 0 && at < user {
						t.Errorf("%s appears before USER, so the process starts as root", strings.TrimSpace(entry))
					}
				}

				// (4) no build arguments: no property carries one, and a
				// Dockerfile that accepted one would be the place a secret
				// gets baked into a layer.
				if regexp.MustCompile(`(?m)^ARG\s`).MatchString(got) {
					t.Error("Dockerfile declares an ARG")
				}
				// Nor a secret mount, which is the other way one arrives.
				if strings.Contains(got, "--mount=type=secret") {
					t.Error("Dockerfile mounts a secret")
				}
			})
		}
	}
}

// TestGenerateRejectsUnpinnedInput covers what the generator refuses.
//
// A version with no pinned digest is refused rather than approximated: a
// fallback to a nearby version, or to a floating tag, would build something
// other than what the Specification asked for.
func TestGenerateRejectsUnpinnedInput(t *testing.T) {
	tests := []struct {
		name    string
		spec    BuildSpec
		wantErr error
	}{
		{
			name:    "a runtime outside the enum",
			spec:    BuildSpec{Stack: Stack{Runtime: "rust", Version: "1.80"}},
			wantErr: ErrUnsupportedRuntime,
		},
		{
			name:    "an empty runtime",
			spec:    BuildSpec{Stack: Stack{Version: "1.22"}},
			wantErr: ErrUnsupportedRuntime,
		},
		{
			name:    "a version with no pinned digest",
			spec:    BuildSpec{Stack: Stack{Runtime: RuntimeGo, Version: "1.19"}},
			wantErr: ErrUnsupportedVersion,
		},
		{
			// "1.22.4" is not "1.22": the table is keyed by what it pins,
			// and a near miss is still a miss.
			name:    "a patch version",
			spec:    BuildSpec{Stack: Stack{Runtime: RuntimeGo, Version: "1.22.4"}},
			wantErr: ErrUnsupportedVersion,
		},
		{
			name:    "an empty version",
			spec:    BuildSpec{Stack: Stack{Runtime: RuntimeNode}},
			wantErr: ErrUnsupportedVersion,
		},
		{
			name:    "a port out of range",
			spec:    BuildSpec{Stack: Stack{Runtime: RuntimeGo, Version: "1.22"}, Ports: []int{70000}},
			wantErr: ErrPortOutOfRange,
		},
		{
			name:    "a zero port",
			spec:    BuildSpec{Stack: Stack{Runtime: RuntimeGo, Version: "1.22"}, Ports: []int{0}},
			wantErr: ErrPortOutOfRange,
		},
	}

	gen := NewDockerfileGenerator()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := gen.Generate(tt.spec)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Generate() error = %v, want %v", err, tt.wantErr)
			}
			if got != "" {
				t.Errorf("Generate() returned a Dockerfile alongside an error:\n%s", got)
			}
		})
	}
}

// TestEveryPinnedRuntimeHasATemplate keeps the two tables that describe a
// runtime from drifting apart. A runtime in one and not the other is a
// Generate that fails on input the enum accepts.
func TestEveryPinnedRuntimeHasATemplate(t *testing.T) {
	for _, r := range SupportedRuntimes() {
		if _, ok := templates[r]; !ok {
			t.Errorf("runtime %q is pinned but has no template", r)
		}
		if len(SupportedVersions(r)) == 0 {
			t.Errorf("runtime %q is pinned with no versions", r)
		}
	}
	for r := range templates {
		if _, ok := baseImages[r]; !ok {
			t.Errorf("runtime %q has a template but no pinned base image", r)
		}
	}
	// And the enum itself: a Runtime the schema accepts must be buildable.
	for _, r := range []Runtime{RuntimeGo, RuntimeNode, RuntimePython, RuntimeJava} {
		if _, ok := baseImages[r]; !ok {
			t.Errorf("runtime %q is in the schema enum but is not pinned", r)
		}
	}
}

// TestGenerateIsDeterministic: the same spec must produce the same bytes.
// A build that is byte-different for no reason is a service that redeploys
// for no reason (RFC 018 §7.5).
func TestGenerateIsDeterministic(t *testing.T) {
	gen := NewDockerfileGenerator()
	spec := BuildSpec{Stack: Stack{Runtime: RuntimeGo, Version: "1.22"}, Ports: []int{8080, 9090}}

	first, err := gen.Generate(spec)
	if err != nil {
		t.Fatalf("Generate() = %v, want nil", err)
	}
	for i := 0; i < 10; i++ {
		again, err := gen.Generate(spec)
		if err != nil {
			t.Fatalf("Generate() = %v, want nil", err)
		}
		if again != first {
			t.Fatalf("Generate() is not deterministic:\n%s\n---\n%s", first, again)
		}
	}
}
