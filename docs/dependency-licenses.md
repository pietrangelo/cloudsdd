# Third-Party Dependency License Audit

CloudSDD's own code is licensed under the **GNU Affero General Public
License v3.0 (or later)** — see [`LICENSE`](../LICENSE) at the repository
root. This document records the compatibility audit performed on every
external Go module the project depends on, directly or transitively.

## Methodology

The audit was produced with [`google/go-licenses`](https://github.com/google/go-licenses)
against two build configurations, so that both the shipped binary and the
integration-test-only dependencies (Docker/testcontainers-go, used solely by
`internal/provider/aws/integration_test.go`, gated behind the `integration`
build tag and never compiled into a release artifact) are covered:

```sh
# Production build graph
go-licenses report ./...

# Full graph, including test-only and integration-tagged imports
GOFLAGS="-tags=integration" go-licenses report ./... --include_tests
```

157 unique external modules were resolved and license-classified this way
(0 "Unknown" results among third-party code — the only "Unknown" entries
`go-licenses` reports are CloudSDD's own packages, which are unlicensed at
the per-package level by design, since the license is declared once at the
repository root plus an SPDX header in each source file).

Re-run the commands above and diff against the tables below whenever
`go.mod`/`go.sum` changes, and update this file in the same PR.

## Compatibility verdict

**All 157 dependencies use licenses compatible with re-licensing the
combined work under AGPLv3.** No copyleft-incompatible, source-unavailable,
or otherwise restrictive license (GPL-2.0-only, SSPL, BUSL, Commons Clause,
proprietary, etc.) was found anywhere in the dependency graph.

| License      | Count | Compatibility with AGPLv3 |
|--------------|------:|----------------------------|
| Apache-2.0   |    69 | Compatible. The FSF lists Apache License 2.0 as one-way compatible with GPLv3/AGPLv3: Apache-2.0 code may be incorporated into an AGPLv3 work. This covers the AWS SDK v2, the Pulumi Go SDK and `pulumi-aws`, gRPC, and the OpenTelemetry stack — the bulk of CloudSDD's cloud/infra surface. |
| MIT          |    54 | Compatible. Permissive, no restrictions beyond attribution. |
| BSD-3-Clause |    25 | Compatible. Permissive, no restrictions beyond attribution/non-endorsement. |
| MPL-2.0      |     4 | Compatible. Mozilla Public License 2.0 §3.3 explicitly allows combining MPL-covered files into a "Larger Work" under GPL/LGPL/AGPL; the MPL-covered files themselves remain MPL-2.0 (HashiCorp `errwrap`, `go-multierror`, `go-version`, `hcl/v2`). |
| BSD-2-Clause |     4 | Compatible. Permissive. |
| ISC          |     1 | Compatible. Functionally equivalent to MIT/BSD-2-Clause, explicitly permissive. |

Some modules contain a handful of vendored files under a different
(still-permissive) license than the module's primary one — e.g.
`aws-sdk-go-v2/internal/sync/singleflight` is BSD-3-Clause inside an
otherwise Apache-2.0 module, and `pulumi/pulumi/sdk/v3` bundles a few
MIT/BSD-3-Clause third-party snippets. These do not change the verdict:
every license actually present is permissive or weak-copyleft-with-larger-work
permission, hence AGPLv3-compatible.

## Full module list by license

### Apache-2.0 (69)
github.com/agext/levenshtein, github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream,
github.com/aws/aws-sdk-go-v2/config, github.com/aws/aws-sdk-go-v2/credentials,
github.com/aws/aws-sdk-go-v2/feature/ec2/imds, github.com/aws/aws-sdk-go-v2/internal/configsources,
github.com/aws/aws-sdk-go-v2/internal/endpoints/v2, github.com/aws/aws-sdk-go-v2/internal/v4a,
github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding, github.com/aws/aws-sdk-go-v2/service/internal/checksum,
github.com/aws/aws-sdk-go-v2/service/internal/presigned-url, github.com/aws/aws-sdk-go-v2/service/internal/s3shared,
github.com/aws/aws-sdk-go-v2/service/s3, github.com/aws/aws-sdk-go-v2/service/signin,
github.com/aws/aws-sdk-go-v2/service/sso, github.com/aws/aws-sdk-go-v2/service/ssooidc,
github.com/aws/aws-sdk-go-v2/service/sts, github.com/containerd/errdefs, github.com/containerd/errdefs/pkg,
github.com/containerd/log, github.com/containerd/platforms, github.com/distribution/reference,
github.com/docker/go-connections, github.com/docker/go-units, github.com/go-git/go-billy/v6,
github.com/go-git/go-git/v6, github.com/go-logr/logr, github.com/go-logr/stdr, github.com/golang/glog,
github.com/moby/docker-image-spec, github.com/moby/go-archive, github.com/moby/moby/api,
github.com/moby/moby/client, github.com/moby/patternmatcher, github.com/moby/sys/sequential,
github.com/moby/sys/user, github.com/moby/sys/userns, github.com/moby/term, github.com/modern-go/concurrent,
github.com/modern-go/reflect2, github.com/opencontainers/go-digest, github.com/opencontainers/image-spec,
github.com/opentracing/basictracer-go, github.com/opentracing/opentracing-go, github.com/pjbgf/sha1cd,
github.com/pulumi/pulumi-aws/sdk/v6, github.com/santhosh-tekuri/jsonschema/v5, github.com/spf13/cobra,
github.com/tklauser/numcpus, github.com/uber/jaeger-client-go, github.com/uber/jaeger-lib,
go.opentelemetry.io/auto/sdk, go.opentelemetry.io/collector/featuregate, go.opentelemetry.io/collector/pdata,
go.opentelemetry.io/contrib/bridges/otelslog, go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp,
go.opentelemetry.io/otel, go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc,
go.opentelemetry.io/otel/exporters/otlp/otlptrace, go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc,
go.opentelemetry.io/otel/log, go.opentelemetry.io/otel/metric, go.opentelemetry.io/otel/sdk,
go.opentelemetry.io/otel/sdk/log, go.opentelemetry.io/otel/trace, go.opentelemetry.io/proto/otlp,
google.golang.org/genproto/googleapis/api, google.golang.org/genproto/googleapis/rpc, google.golang.org/grpc.

### MIT (54)
github.com/BurntSushi/toml, github.com/aead/chacha20, github.com/apparentlymart/go-textseg/v13,
github.com/apparentlymart/go-textseg/v15, github.com/aymanbagabas/go-osc52/v2, github.com/blang/semver,
github.com/cenkalti/backoff/v4, github.com/cenkalti/backoff/v5, github.com/cespare/xxhash/v2,
github.com/charmbracelet/bubbles, github.com/charmbracelet/bubbletea, github.com/charmbracelet/colorprofile,
github.com/charmbracelet/lipgloss, github.com/charmbracelet/x/ansi, github.com/charmbracelet/x/cellbuf,
github.com/charmbracelet/x/term, github.com/clipperhouse/displaywidth, github.com/clipperhouse/uax29/v2,
github.com/cpuguy83/dockercfg, github.com/djherbis/times, github.com/felixge/httpsnoop,
github.com/gabriel-vasile/mimetype, github.com/go-playground/locales, github.com/go-playground/universal-translator,
github.com/go-playground/validator/v10, github.com/json-iterator/go, github.com/kevinburke/ssh_config,
github.com/klauspost/compress, github.com/klauspost/cpuid/v2, github.com/leodido/go-urn,
github.com/lucasb-eyer/go-colorful, github.com/mattn/go-isatty, github.com/mattn/go-runewidth,
github.com/mitchellh/go-wordwrap, github.com/muesli/ansi, github.com/muesli/cancelreader,
github.com/muesli/termenv, github.com/nxadm/tail, github.com/pgavlin/fx, github.com/pgavlin/fx/v2,
github.com/pulumi/appdash, github.com/pulumi/pulumi/sdk/v3, github.com/rivo/uniseg, github.com/sergi/go-diff,
github.com/sirupsen/logrus, github.com/stretchr/testify, github.com/testcontainers/testcontainers-go,
github.com/texttheater/golang-levenshtein, github.com/xo/terminfo, github.com/zclconf/go-cty,
go.uber.org/atomic, go.uber.org/multierr, gopkg.in/yaml.v3, lukechampine.com/frand.

### BSD-3-Clause (25)
dario.cat/mergo, github.com/ProtonMail/go-crypto, github.com/aws/aws-sdk-go-v2, github.com/aws/smithy-go,
github.com/cheggaaa/pb, github.com/cloudflare/circl, github.com/fsnotify/fsnotify, github.com/go-git/gcfg/v2,
github.com/gogo/protobuf, github.com/google/uuid, github.com/grpc-ecosystem/grpc-gateway/v2,
github.com/grpc-ecosystem/grpc-opentracing, github.com/pmezard/go-difflib, github.com/rogpeppe/go-internal,
github.com/shirou/gopsutil/v4, github.com/spf13/pflag, github.com/tklauser/go-sysconf, golang.org/x/crypto,
golang.org/x/net, golang.org/x/sync, golang.org/x/sys, golang.org/x/term, golang.org/x/text,
google.golang.org/protobuf, gopkg.in/tomb.v1.

### MPL-2.0 (4)
github.com/hashicorp/errwrap, github.com/hashicorp/go-multierror, github.com/hashicorp/go-version,
github.com/hashicorp/hcl/v2.

### BSD-2-Clause (4)
github.com/emirpasic/gods, github.com/magiconair/properties, github.com/pkg/errors, github.com/pkg/term.

### ISC (1)
github.com/davecgh/go-spew.

## Adding new dependencies

Before adding a new module to `go.mod`, check its license with:

```sh
go-licenses report ./... 2>&1 | grep <new-module-path>
```

Reject (or replace with an alternative) any dependency under a strong
copyleft license without a "larger work" exception (plain GPL-2.0/3.0),
a source-available-but-not-open-source license (SSPL, BUSL, Commons Clause),
or a proprietary license. Flag it for discussion in the relevant RFC instead
of merging silently.
