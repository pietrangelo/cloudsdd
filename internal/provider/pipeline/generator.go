// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package pipeline

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"text/template"
)

// BuildSpec describes the pure, cloud-agnostic requirements for a
// Dockerfile: what to build it with, and which ports the image declares.
type BuildSpec struct {
	Stack Stack
	Ports []int
}

// DockerfileGenerator produces the Dockerfile a build_pipeline builds with.
//
// Following RFC 018 §2.2, implementations MUST ensure:
//  1. The container runs as a non-root user.
//  2. A multi-stage build is used to omit toolchains from the final layer.
//  3. Base images are pinned by digest.
//  4. No build arguments or secrets are injected via the Dockerfile.
//
// The method takes no context because it consults nothing outside its
// argument: no registry, no network, no clock. A context here would
// advertise a cancellability that does not exist. What resolves a tag to a
// digest is this package's pinned table, updated deliberately in review,
// which is the whole point of (3).
type DockerfileGenerator interface {
	// Generate produces a strict, secure Dockerfile for the given spec.
	Generate(spec BuildSpec) (string, error)
}

// Generator errors.
var (
	// ErrUnsupportedRuntime indicates a runtime outside the enum.
	ErrUnsupportedRuntime = errors.New("build_pipeline: unsupported runtime")

	// ErrUnsupportedVersion indicates a runtime version with no pinned base
	// image.
	//
	// Refused rather than substituted. The alternative — falling back to a
	// nearby version, or to a floating tag — would build something other
	// than what the Specification asked for, which is the same defect RFC
	// 017 §2.4 refuses in an image reference. The supported set is small
	// because every entry is a digest somebody has to keep current.
	ErrUnsupportedVersion = errors.New("build_pipeline: unsupported runtime version")

	// ErrPortOutOfRange indicates a port no image could listen on.
	ErrPortOutOfRange = errors.New("build_pipeline: port out of range")
)

// stages names the two pinned base images of a multi-stage build.
//
// Both are full references including a digest. The tag is kept alongside
// the digest deliberately: a digest alone is unreadable in review, and the
// tag says what the digest was resolved from. Docker resolves by digest and
// ignores the tag, so the tag is documentation and cannot change what is
// pulled.
type stages struct {
	// build carries the toolchain, and never ships.
	build string
	// run is the final stage: distroless where the artifact is
	// self-contained, the official slim variant where a runtime is needed.
	run string
}

// baseImages is the pinned base image table (RFC 018 §2.2).
//
// This is the file that must be updated to take a base image update,
// deliberately and in review. Generating a Dockerfile whose FROM was a
// mutable tag would be the same defect RFC 017 §2.4 refuses one level down:
// what gets built would not be what was reviewed.
//
// Digests are the multi-platform index digest, so one pin is correct on
// every architecture a build service might run on.
//
// Resolved 2026-08-08. To refresh one, resolve the tag on the right and
// replace the digest; do not add a version without adding its digest.
var baseImages = map[Runtime]map[string]stages{
	RuntimeGo: {
		// A Go binary is self-contained, so the final stage carries no
		// distribution at all: no shell, no package manager, nothing for a
		// process that escapes the application to use.
		"1.22": {
			build: "golang:1.22-bookworm@sha256:3d699e4d15d0f8f13c9195c0632a16702b8cbdece2955af1c23b37ae5d55a253",
			run:   distrolessStatic,
		},
		"1.23": {
			build: "golang:1.23-bookworm@sha256:167053a2bb901972bf2c1611f8f52c44d5fe7e762e5cab213708d82c421614db",
			run:   distrolessStatic,
		},
		"1.24": {
			build: "golang:1.24-bookworm@sha256:1a6d4452c65dea36aac2e2d606b01b4a029ec90cc1ae53890540ce6173ea77ac",
			run:   distrolessStatic,
		},
	},
	RuntimeNode: {
		"20": {
			build: "node:20-bookworm@sha256:8f693eaa7e0a8e71560c9a82b55fd54c2ae920a2ba5d2cde28bac7d1c01c9ba5",
			run:   "node:20-bookworm-slim@sha256:2cf067cfed83d5ea958367df9f966191a942351a2df77d6f0193e162b5febfc0",
		},
		"22": {
			build: "node:22-bookworm@sha256:0557ac14e0d45d02ed563067b82856ca5e7aa3437fa28d98d4350ea9c3d9494a",
			run:   "node:22-bookworm-slim@sha256:d649c27dae7ba0137b3cef5dd75baa422c08dc3d9e3fc0c23dfb172dc3cc6436",
		},
		"24": {
			build: "node:24-bookworm@sha256:934240a162082fd8b8a2f90cd5114446443f1eba1c5378f6687167ca405e6584",
			run:   "node:24-bookworm-slim@sha256:3638d9a6fe4030bd716be989438248074489337ba3275657f93595428be4fc03",
		},
	},
	RuntimePython: {
		"3.11": {
			build: "python:3.11-bookworm@sha256:a8f8fbe1a0edc9e4dddafa64ba73f7e04be7be5ebc23f332362e779e0a2e4e52",
			run:   "python:3.11-slim-bookworm@sha256:d29f48a31a8b408ed19272ca1e7b10ebae13b240a27e862d3d4217c528e2e0c3",
		},
		"3.12": {
			build: "python:3.12-bookworm@sha256:3cd9086bdb30f7c9bc08a3fa621d9842e0d3f6f9291aeb4677e0547817c10b12",
			run:   "python:3.12-slim-bookworm@sha256:4766d8b510c428e595d74b9cc5bbb2fae8e26316fffb4adc89908d79aacd58a2",
		},
		"3.13": {
			build: "python:3.13-bookworm@sha256:8b9a8b28d9cc221c6ab5d40e9cfcd99429959f6a8f5171612a99147975ab043f",
			run:   "python:3.13-slim-bookworm@sha256:67a1e1f215ccda113cfc024e8639049257e88f273898f595b61476d128d387e8",
		},
	},
	RuntimeJava: {
		// A jar needs a JVM but nothing else, so distroless applies here
		// too — with the JRE variant matching the JDK that built it.
		"17": {
			build: "eclipse-temurin:17-jdk@sha256:abb3826b404269a005829b63e2e7bd48a7be32115ab7ba9fa0d8cba834360eef",
			run:   "gcr.io/distroless/java17-debian12:nonroot@sha256:06484c2a9dcc9070aeafbc0fe752cb9f73bc0cea5c311f6a516e9010061998ad",
		},
		"21": {
			build: "eclipse-temurin:21-jdk@sha256:efd34b940f2d5a621605c8531c2afb7759c936b6c2ef637a69aa3bf3e1e789d1",
			run:   "gcr.io/distroless/java21-debian12:nonroot@sha256:7e37784d94dccbf5ccb195c73b295f5ad00cd266512dfbac12eb9c3c28f8077d",
		},
	},
}

// distrolessStatic is the final stage for a statically linked binary,
// shared by every Go version because it carries nothing version-specific.
const distrolessStatic = "gcr.io/distroless/static-debian12:nonroot@sha256:f5b485ea962d9bd1186b2f6b3a061191539b905b82ec395de78cbfae51f20e35"

// nonRootUID is the unprivileged user the image runs as, matching the uid
// distroless's `nonroot` user already has. The two runtimes that build
// their own user use the same number so that a volume written by one image
// is readable by another (RFC 018 §2.7).
const nonRootUID = 65532

// SupportedVersions lists the versions pinned for a runtime, sorted. It is
// what an error message quotes, and what documentation is generated from,
// so that the answer has one source.
func SupportedVersions(r Runtime) []string {
	versions := make([]string, 0, len(baseImages[r]))
	for v := range baseImages[r] {
		versions = append(versions, v)
	}
	sort.Strings(versions)
	return versions
}

// SupportedRuntimes lists the runtimes with pinned base images, sorted.
func SupportedRuntimes() []Runtime {
	runtimes := make([]Runtime, 0, len(baseImages))
	for r := range baseImages {
		runtimes = append(runtimes, r)
	}
	sort.Slice(runtimes, func(i, j int) bool { return runtimes[i] < runtimes[j] })
	return runtimes
}

// generator is the built-in DockerfileGenerator.
type generator struct{}

// NewDockerfileGenerator returns the generator that backs every provider.
//
// It is a value with no configuration on purpose: a generator that could be
// configured would be a generator whose output depends on something other
// than the Specification, and the plan shows the Dockerfile precisely so
// that what a user approves is what gets built (RFC 018 §2.2).
func NewDockerfileGenerator() DockerfileGenerator { return generator{} }

var _ DockerfileGenerator = generator{}

// templateData is what the per-runtime templates render.
type templateData struct {
	Build   string
	Run     string
	Ports   []int
	UID     int
	Runtime Runtime
	Version string
}

// Generate renders the Dockerfile for spec.
func (generator) Generate(spec BuildSpec) (string, error) {
	versions, ok := baseImages[spec.Stack.Runtime]
	if !ok {
		return "", fmt.Errorf("%w: %q (supported: %v)",
			ErrUnsupportedRuntime, spec.Stack.Runtime, SupportedRuntimes())
	}
	base, ok := versions[spec.Stack.Version]
	if !ok {
		return "", fmt.Errorf("%w: %s %q (supported: %v)",
			ErrUnsupportedVersion, spec.Stack.Runtime, spec.Stack.Version,
			SupportedVersions(spec.Stack.Runtime))
	}
	for _, port := range spec.Ports {
		if port < 1 || port > 65535 {
			return "", fmt.Errorf("%w: %d", ErrPortOutOfRange, port)
		}
	}

	tmpl, ok := templates[spec.Stack.Runtime]
	if !ok {
		// Unreachable while baseImages and templates agree, which
		// TestEveryPinnedRuntimeHasATemplate is what keeps true.
		return "", fmt.Errorf("%w: %q has no template", ErrUnsupportedRuntime, spec.Stack.Runtime)
	}

	var out strings.Builder
	err := tmpl.Execute(&out, templateData{
		Build:   base.build,
		Run:     base.run,
		Ports:   spec.Ports,
		UID:     nonRootUID,
		Runtime: spec.Stack.Runtime,
		Version: spec.Stack.Version,
	})
	if err != nil {
		return "", fmt.Errorf("build_pipeline: failed to render the Dockerfile: %w", err)
	}
	return out.String(), nil
}

// header is prepended to every generated Dockerfile.
//
// It is addressed to the person reviewing a plan, which is the only reason
// this file is shown rather than hidden (RFC 018 §2.2). "CloudSDD
// generated something sensible" is not something a reviewer can check, so
// the file states what it assumed about the repository — the one thing a
// reader cannot infer from the Dockerfile alone when they are looking at it
// before the first build.
const header = `# Generated by CloudSDD (RFC 018) for {{.Runtime}} {{.Version}}. Do not edit:
# this file is regenerated on every build and is not read from the
# repository. Base images are pinned by digest in
# internal/provider/pipeline/generator.go.
#
{{block "contract" .}}{{end}}
`

// exposeBlock is shared by every template. EXPOSE is documentation to the
// platform rather than an opening: nothing routes to these ports except the
// one the container_service names (RFC 018 §2.3).
const exposeBlock = `{{range .Ports}}
EXPOSE {{.}}{{end}}`

var templates = map[Runtime]*template.Template{
	RuntimeGo:     mustParse(RuntimeGo, goTemplate),
	RuntimeNode:   mustParse(RuntimeNode, nodeTemplate),
	RuntimePython: mustParse(RuntimePython, pythonTemplate),
	RuntimeJava:   mustParse(RuntimeJava, javaTemplate),
}

func mustParse(r Runtime, body string) *template.Template {
	t, err := template.New(string(r)).Parse(header + body)
	if err != nil {
		// A malformed template is a bug in this file, not a runtime
		// condition — the same reasoning internal/spec/validate.go applies
		// to a validator tag.
		panic(fmt.Sprintf("pipeline: failed to parse the %s Dockerfile template: %v", r, err))
	}
	return t
}

// The build stage carries the toolchain and the module cache; the final
// stage carries a static binary and nothing else — no shell and no package
// manager for a process that escapes the application to reach for.
const goTemplate = `{{define "contract"}}# Expects: a main package at the repository root, plus go.mod (and go.sum).{{end}}
FROM {{.Build}} AS build
WORKDIR /src

# Dependencies first, so a source-only change does not re-download them.
# The bracket makes go.sum optional: a module with no dependencies has none,
# and a COPY that fails on its absence would refuse to build one.
COPY go.mod go.su[m] ./
RUN go mod download

COPY . .
# CGO off is what makes the binary static, and therefore what makes the
# distroless final stage possible. -trimpath keeps build paths out of the
# binary; -s -w drops the symbol table.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app .

FROM {{.Run}}
COPY --from=build /out/app /app
` + exposeBlock + `
USER {{.UID}}:{{.UID}}
ENTRYPOINT ["/app"]
`

// npm ci in the build stage, and only the resulting node_modules crosses
// into the final image: the slim variant has no compiler for a native
// module to rebuild with, which is the point.
const nodeTemplate = `{{define "contract"}}# Expects: package.json with a "start" script, and package-lock.json.{{end}}
FROM {{.Build}} AS build
WORKDIR /src

COPY package.json package-lock.json ./
# --omit=dev keeps test and build tooling out of what ships. ci rather than
# install so the lockfile decides, not the registry's newest match — which
# is also why the lockfile is copied rather than treated as optional.
RUN npm ci --omit=dev

COPY . .

FROM {{.Run}}
# The official image carries a "node" user, but at uid 1000. A user is
# created here instead so that every runtime CloudSDD generates shares one
# uid and a volume written by one image stays readable by another (RFC 018
# §2.7).
RUN groupadd --system --gid {{.UID}} app \
 && useradd --system --uid {{.UID}} --gid app --no-create-home --shell /usr/sbin/nologin app

WORKDIR /app
COPY --from=build --chown={{.UID}}:{{.UID}} /src /app
ENV NODE_ENV=production
` + exposeBlock + `
USER {{.UID}}:{{.UID}}
ENTRYPOINT ["npm", "start"]
`

// The virtualenv is what crosses stages: pip, its cache and any build
// toolchain a wheel needed stay in the build stage.
const pythonTemplate = `{{define "contract"}}# Expects: requirements.txt and main.py at the repository root.{{end}}
FROM {{.Build}} AS build
WORKDIR /src

RUN python -m venv /opt/venv
ENV PATH="/opt/venv/bin:$PATH"

COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt

COPY . .

FROM {{.Run}}
# The slim image ships no unprivileged user, so one is created here. The uid
# matches distroless's "nonroot" so a shared volume stays readable across
# runtimes (RFC 018 §2.7).
RUN groupadd --system --gid {{.UID}} app \
 && useradd --system --uid {{.UID}} --gid app --no-create-home --shell /usr/sbin/nologin app

WORKDIR /app
COPY --from=build /opt/venv /opt/venv
COPY --from=build --chown={{.UID}}:{{.UID}} /src /app

ENV PATH="/opt/venv/bin:$PATH" \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1
` + exposeBlock + `
USER {{.UID}}:{{.UID}}
ENTRYPOINT ["python", "main.py"]
`

// The JDK builds; the JRE runs. The distroless java image's own entrypoint
// is `java -jar`, so the command is the jar and nothing else.
const javaTemplate = `{{define "contract"}}# Expects: a Maven project with the wrapper (./mvnw) producing target/*.jar.{{end}}
FROM {{.Build}} AS build
WORKDIR /src

COPY . .
# Tests run in CI, not in the image build: a build that reruns them here
# would fail deployments for reasons that have nothing to do with the image.
RUN ./mvnw --batch-mode --no-transfer-progress -DskipTests package

FROM {{.Run}}
WORKDIR /app
COPY --from=build /src/target/*.jar /app/app.jar
` + exposeBlock + `
USER {{.UID}}:{{.UID}}
CMD ["/app/app.jar"]
`
