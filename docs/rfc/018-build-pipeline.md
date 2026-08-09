# RFC 018: Build Pipeline

- **Status:** Proposed
- **Author:** Claude (Senior Staff Cloud Platform Engineer, AI-assisted)
- **Date:** 2026-08-02
- **Depends on:** [RFC 016](016-environment-network.md),
  [RFC 017](017-container-service.md) — implemented 2026-08-02

## 1. Problem

RFC 017 shipped `container_service` on all three clouds and left one thing
entirely outside CloudSDD: **where the image comes from**. The user names
an image, CloudSDD refuses it unless it is pinned or allow-listed, and
then runs it. Building it is somebody else's job, done somewhere else,
with none of the defaults this project exists to enforce.

That is a real gap rather than a tidy boundary. The mandate says the user
describes intent and the system supplies the state of the art without
being asked. Today that holds for the network, the perimeter, the
identity and the encryption around a container — and stops at the
container itself, whose contents are whatever a `Dockerfile` somebody
wrote happens to produce: running as root, built on a base image nobody
audited, with build-time secrets baked into a layer.

It is also the last thing standing between CloudSDD and a description that
starts from source: *"deploy the Go service in this repository"* is what a
user actually wants to say, and today they must build and push an image
first, by hand, and then describe the result.

## 2. Proposed Architecture

### 2.1 A new `ResourceType`: `build_pipeline`

A pipeline is a resource with its own lifecycle — it is created, it runs,
it is destroyed — so it is a `ResourceType` rather than a property of
`container_service`. Two services can be fed by one pipeline; a pipeline
can exist before any service consumes it.

```json
{
  "id": "api-build",
  "type": "build_pipeline",
  "provider": "agnostic",
  "scope": { "region": "eu-central-1", "environment": "prod" },
  "properties": {
    "source": {
      "repository": "https://github.com/acme/api",
      "revision": "main"
    },
    "stack": { "runtime": "go", "version": "1.22" },
    "image_name": "acme/api",
    "ports": [8080, 9090],
    "retain": 5
  }
}
```

| Property | Type | Default | Notes |
|---|---|---|---|
| `source.repository` | string, required | — | HTTPS URL of a **public** Git repository (§2.6) |
| `source.revision` | string, required | — | Branch, tag or commit SHA; resolved to a commit at plan time (§2.4) |
| `stack.runtime` | enum, required | — | `go`, `node`, `python`, `java`; §2.2 |
| `stack.version` | string, required | — | Runtime version, e.g. `1.22` |
| `image_name` | string, required | — | Repository name within the registry CloudSDD creates |
| `ports` | list of int | `[]` | Ports the image declares; §2.3 |
| `retain` | int | `5` | How many images to keep; §2.5 |

### 2.2 `stack` means a generated Dockerfile, not a supplied one

The user names a runtime and a version. CloudSDD generates the
`Dockerfile`, which is what makes the mandate's "state-of-the-art
configuration without asking" apply to the image's *contents* rather than
only to its surroundings:

- **a non-root user**, created in the image and switched to before the
  entrypoint. The single most common container misconfiguration, and one
  a user cannot opt into forgetting;
- **a multi-stage build**, so the toolchain, the module cache and the
  source tree stay out of the shipped layer. A Go binary ships on a
  distroless base; an interpreted runtime ships on the slim variant of its
  official image;
- **no build arguments and no secrets**, because there is no property to
  put one in — the same line RFC 017 §2.2 drew for `env`, for the same
  reason. A secret in a `--build-arg` is a secret in an image layer,
  readable by anyone who can pull it;
- **a pinned base image**, by digest, recorded in this repository and
  updated deliberately. RFC 017 §2.4 refuses a mutable tag in a
  Specification; generating a `Dockerfile` whose `FROM` is a mutable tag
  would be the same defect one level down.

The generated `Dockerfile` is **shown in the plan**, not hidden. A user
approving a build has to be able to see what will be built, and "CloudSDD
generated something sensible" is not something a reviewer can check.

The runtime enum is deliberately small and holds only what all three
build services can produce. A runtime that worked on one cloud would make
`agnostic` resolution silently provider-specific, which is the coupling
RFC 013 §7.2 rejected for `compute_instance` images.

### 2.3 `ports` describes the image; `container_service.port` still routes

The original request asks for "the port or ports to expose". Those are two
different things, and conflating them would produce a Specification that
cannot be honoured:

- **Cloud Run accepts one ingress port. Container Apps accepts one
  `targetPort`.** Only an ALB could route more, so a multi-port *ingress*
  would be an AWS-only capability wearing a cloud-agnostic name.

So `ports` on the pipeline is what the image **declares** (`EXPOSE`), and
it is the set a service may choose from. `container_service.port` stays a
single port and must be one of them — checked at the Engine, where a
mismatch is caught before anything is built. A service on a port the image
never exposes is a deployment that starts and answers nothing.

Ports beyond the ingress one are reachable from the scope's network, which
is what makes a metrics or admin port expressible without exposing it.

### 2.4 The pin moves from the image to the commit

RFC 017 §2.4 is the part of this design most at risk. It requires an image
pinned to a digest unless its registry is allow-listed, and refuses
`:latest` outright, because **what runs must be what was reviewed**. A
pipeline cannot satisfy that as written: the digest does not exist until
the build runs, so no Specification could name it.

The resolution is not to weaken the rule but to move where it applies.
Provenance for a built image comes from its *source*, so:

- `source.revision` may be a branch, a tag or a commit SHA;
- **at plan time CloudSDD resolves it to a commit SHA and shows it**, so
  the user approves a specific commit rather than "whatever is newest";
- every build is tagged with that SHA — never `latest`, on the same
  reasoning §2.4 gave;
- a `container_service` consuming the pipeline receives the **digest** the
  build produced.

A branch is therefore allowed where a `:latest` tag is not, and the
difference is real: `:latest` is resolved by the registry at pull time,
after review, and can change again afterwards. A branch is resolved by
CloudSDD *before* review, and what is approved is a commit.

`policies.allowed_registries` is untouched: an image built by CloudSDD
comes from the registry CloudSDD created, and a user who has allow-listed
registries has allow-listed that one implicitly, because it is theirs.

### 2.4.1 How the service learns which image to run

§2.4 says a `container_service` "receives the digest the build produced"
without saying by what mechanism, and the obvious readings do not work.
CloudSDD runs **one Pulumi stack per `Resource.ID`**, so the service's
program cannot see the pipeline's outputs; and the reference carries only
an id, not a registry, a repository or a commit.

**The commit SHA is the mechanism.** It is already the thing §2.4
resolves and shows, and it is known to both sides before either is
applied:

1. at plan time CloudSDD resolves `source.revision` to a commit SHA;
2. the build tags the image with that SHA;
3. the service asks for `<registry>/<image_name>:<sha>`.

Nothing has to be read back out of a build that has not run yet, and no
cross-stack reference is needed. What makes the tag as strong as a digest
is that **the registry is created with immutable tags** (§2.8): a tag that
cannot be repointed names one artifact forever, which is the property
§2.4 actually wants — what runs is what was reviewed.

The registry host is assembled by the **provider**, because it is the only
component that knows it: an ECR host contains the account id, an Artifact
Registry path contains the project, an ACR login server is named after the
registry. The Engine supplies what it knows — the resolved commit and the
pipeline's `image_name` — through a field on `spec.Resource` that carries
no JSON tag, so it can never be set from a Specification.

Revision resolution speaks Git's smart-HTTP ref advertisement directly,
rather than shelling out to `git` or taking a Git library as a dependency.
It is one unauthenticated GET returning every ref and its object name,
which is exactly the question being asked, and it is host-agnostic where a
forge REST API would work on one host only.

A revision that is already a full commit SHA is passed through without a
lookup, so a fully pinned Specification plans with no network access.
Abbreviated SHAs are not accepted: an abbreviation is a prefix, and
prefixes collide.

### 2.5 Retention

Five images by default, configurable, enforced by the registry's own
lifecycle mechanism rather than by anything CloudSDD runs:

| | Registry | Mechanism |
|---|---|---|
| AWS | ECR | Lifecycle policy, `imageCountMoreThan` |
| GCP | Artifact Registry | Cleanup policy, keep most-recent-versions |
| Azure | ACR | Retention policy — **Premium SKU only**, see §7.2 |

Retention counts *images*, not tags. A rule that kept five tags would keep
five names and an unbounded number of untagged manifests behind them,
which is the storage bill this exists to bound.

**The running image is never reaped.** A retention policy that can delete
what a service is currently pulling is an outage waiting for a scale-up
event, so the digest a `container_service` references is excluded from the
policy.

### 2.6 Public repositories only, in this RFC

A private repository needs a credential: a CodeStar connection on AWS, a
Cloud Build repository link on GCP, a PAT for ACR Tasks. Each is a
long-lived secret, and the only place a Specification could carry one is a
property — which `internal/provider/decode`'s denylist rejects by name,
correctly.

So this RFC builds from **public repositories only**, and private-source
support is deferred to the RFC that can pair it with a secrets story. That
is the same line RFC 017 §2.2 drew for `env` and for the same reason: the
feature is not the hard part, and shipping it without the secrets story is
how the credential ends up in the document.

This is a real limitation and it is stated here rather than discovered:
most services worth deploying live in private repositories.

### 2.7 Volumes: the portable subset, and what is deferred

The request names S3 and EFS, "and the equivalents for GCP and Azure".
Those two are not equivalents of each other, and the clouds do not offer
them symmetrically:

| | Filesystem | Object storage as a mount |
|---|---|---|
| AWS, ECS Fargate | **EFS**, a native volume type | S3 is not a Fargate volume type; it needs Mountpoint in a sidecar |
| GCP, Cloud Run | **Filestore** (NFS), native | **GCS via FUSE**, native |
| Azure, Container Apps | **Azure Files**, native | Blob has no native mount |

So this RFC supports **filesystem volumes on all three** — EFS, Filestore,
Azure Files — and defers object-storage mounts.

Two reasons, and the second matters more than the first. It is native on
one cloud of three, so offering it would mean CloudSDD shipping and
patching a sidecar or a CSI driver on the other two: a deployed code
artifact, which RFC 012 §4.1 avoided for scheduling on exactly this
reasoning. And object storage mounted as a filesystem *is not one* — no
atomic rename, no locking, directory listings that cost money and lie
under concurrency. Anything written against a POSIX mount misbehaves on it
in ways that surface as data loss rather than as errors. A user who wants
S3 should get an SDK and a bucket, which `object_storage` already
provides.

```json
"volumes": [
  { "name": "uploads", "mount_path": "/var/lib/uploads", "size_gb": 100 }
]
```

The volume belongs to the **scope**, not to the service, so two services in
one environment can share one — which is the point of a shared filesystem
and is why it is not a property of `container_service`.

### 2.8 Per-provider mapping

| | AWS | GCP | Azure |
|---|---|---|---|
| Build | CodeBuild | Cloud Build | ACR Tasks |
| Registry | ECR | Artifact Registry | ACR |
| Retention | ECR lifecycle policy | Cleanup policy | Purge task, `--keep` (§2.8.1) |
| Filesystem | EFS | Filestore | Azure Files |
| Build identity | Service role, scoped to one ECR repository | Service account, no project roles | Task identity |

The build identity gets **write access to one repository and nothing
else**, following the RFC 017 §2.6 precedent. A build role that can push
anywhere is a build role that can replace any image in the account.

### 2.8.1 Azure: retention and tag immutability — decided

§2.5 and §2.4.1 were written against ECR and Artifact Registry, and ACR
does not have the two features they assume. This section records what
Azure does instead, because both gaps are real and neither is visible from
the table above.

**Retention: a purge task, not the native retention policy.** §7.2 framed
the choice as Premium-or-nothing. That framing was wrong on a fact:
ACR's native retention policy is `{days, enabled}` — **age-based, and it
reaps only untagged manifests**. It cannot express "keep the most recent
`retain` images" the way `imageCountMoreThan` and
`mostRecentVersions` do, and because this pipeline pushes one commit tag
per build there are almost no untagged manifests for it to find. Buying
Premium — roughly five times Basic — would therefore have paid for a
policy that deletes close to nothing, while leaving `retain` decoded and
unhonoured.

So Azure keeps a **Basic** registry and honours `retain` with a
timer-triggered ACR Task running `acr purge --keep <retain>`. That is the
scheduled task §7.2 was reluctant about, and the reluctance is worth
answering: `acr purge` is a first-party Microsoft command shipped as
`mcr.microsoft.com/acr/acr-cli`, not code CloudSDD writes, deploys or
patches — the distinction §2.7 drew against shipping a CSI driver. This
package already declares an Azure Automation runbook for RFC 012's power
schedule on the same terms.

It also honours `retain` **better** than the Premium option would: the
purge counts images and keeps the newest `retain` of them, which is what
§2.5 asks for and what the other two clouds do.

`retain`'s exclusion of a running image is not expressible here either, on
the same terms recorded for AWS and GCP: purge selects on a repository
filter, an age and a count, and cannot ask what is deployed.

**Tag immutability: not available, and the guarantee is weaker.** §2.4.1
rests the commit tag's strength on the registry being created with
immutable tags. **ACR has no registry-wide immutable-tags setting** — it
is a per-repository lock applied after the fact, not a property of the
registry — so the Azure registry cannot offer it and the tag is
technically movable.

What holds instead: the registry is created by CloudSDD for this pipeline
alone, and the only principal that can push to it is the pipeline's own
task, which only ever pushes `<image_name>:<commit>` built from that
commit. Repointing a reviewed tag at a different artifact therefore
requires push access to a registry nothing else has push access to. That
is a weaker guarantee than ECR's and Artifact Registry's — it is enforced
by who holds the credential rather than by the registry refusing the
write — and it is stated here rather than discovered.

### 2.9 This introduces resource references

`container_service` gains `pipeline`, naming a `build_pipeline` in the
same Specification. That makes it **the first cross-resource reference the
schema has**, which `architecture.md` currently lists among the things
that do not exist — and it is why `Destroy` walks the resource list in
reverse rather than in dependency order.

This RFC therefore also introduces:

- **reference validation** at the Engine: a `pipeline` naming a resource
  that is absent, or that is not a `build_pipeline`, is an error before
  anything is applied;
- **ordering**: a pipeline is applied, and its build completed, before any
  service that consumes it;
- **cycle refusal**, trivially today with one reference kind, and stated
  now because it stops being trivial the moment there is a second.

Introducing a general dependency graph is out of scope. One typed
reference with explicit ordering is what this RFC needs, and a graph
should be designed by the RFC that has two reasons for one.

## 3. Impacted JSON Schema

- `build_pipeline` joins the `ResourceType` enum — the first addition
  since RFC 001;
- `BuildPipelineProperties` in `docs/openapi.yaml`;
- `container_service` gains `pipeline` (string, optional) and `image`
  becomes optional — **exactly one of the two is required**, and both
  together is an error rather than a precedence rule;
- `policies.network` gains nothing; volumes are addressed from the scope's
  existing range;
- the system prompt gains the type, the runtime enum, and the instruction
  that a repository URL is carried through and never invented — the same
  rule RFC 017 §3 set for `image`, and for the same reason.

## 4. Security Considerations

| Threat | Mitigation |
|---|---|
| Build-time secrets baked into a layer | No property carries one: no build args, no `env` on the pipeline (§2.2) |
| Container runs as root | The generated Dockerfile creates a user and switches to it before the entrypoint; not optional (§2.2) |
| Toolchain and source shipped in the runtime image | Multi-stage build, distroless or slim final stage (§2.2) |
| Unaudited base image, or one that changes under us | Base images pinned by digest in this repository, updated deliberately (§2.2) |
| "Deploy whatever is newest" reintroduced through a branch | The revision is resolved to a commit at plan time and shown; the build is tagged with the SHA, never `latest` (§2.4) |
| A build role that can replace any image in the account | Push access scoped to the one repository the pipeline owns (§2.8) |
| Source credential in the Specification | Public repositories only in this RFC; private sources deferred to a secrets story (§2.6) |
| Retention deleting a running image | The digest a live service references is excluded from the policy (§2.5) |
| A service routed to a port the image never exposes | `container_service.port` must appear in the pipeline's `ports`, checked at the Engine (§2.3) |
| Arbitrary code executed at build time from an untrusted repository | The build runs in the provider's managed, ephemeral build environment with the scoped identity above — it is the user's own repository, and the blast radius is one registry repository |

## 5. Testing Plan

Table-driven, `pulumi.WithMocks` for declaration, no Docker and no cloud.

1. **Dockerfile generation**, per runtime: a non-root user is created and
   `USER` is set before the entrypoint; the final stage is distroless or
   slim; every `FROM` names a digest; no `ARG` appears. Golden files, so a
   change to a generated Dockerfile is visible in review.
2. **Property decoding**: required `source`, `stack`, `image_name`;
   runtime enum; `retain` bounds; unknown properties rejected; a
   credential-shaped property rejected.
3. **The reference**, at the Engine: `pipeline` naming an absent resource,
   naming a resource of the wrong type, and naming one correctly; `image`
   and `pipeline` both set; neither set.
4. **Port agreement**: a `container_service.port` absent from the
   pipeline's `ports` is refused before anything is built.
5. **Retention**, per provider: the policy keeps `retain` images, counts
   images rather than tags, and excludes the referenced digest.
6. **Build identity**: the declared role can push to one repository and
   carries no other permission.
7. **Ordering**: a Specification with a service and its pipeline applies
   the pipeline first, and a `Destroy` removes the service first.
8. **Volumes**: the declared filesystem is in the scope's network, is
   mounted at the requested path, and is not created when no volume is
   declared.

## 6. Rollout

1. `internal/provider/pipeline` — the shared shape, the runtime enum, and
   the Dockerfile generator with its golden files. Pure, no cloud.
2. The `build_pipeline` type, the `pipeline` reference on
   `container_service`, reference validation and ordering at the Engine.
3. AWS: CodeBuild, ECR, the lifecycle policy, the scoped role.
4. GCP: Cloud Build, Artifact Registry, the cleanup policy.
5. Azure: ACR Tasks, ACR, the retention policy — and §7.2's answer.
6. Volumes on all three.
7. `cli.md`, `architecture.md`, `openapi.yaml`, and the system prompt.

Steps 1 and 2 are the ones worth reviewing hardest: everything after them
is three translations of decisions already made.

## 7. Open Questions

1. **Who triggers a build?** This RFC provisions the pipeline and runs it
   on apply, so `cloudsdd deploy` builds and deploys. It does **not**
   install a webhook, so a push to the branch does not rebuild. That keeps
   CloudSDD's model — infrastructure changes when you ask for it — but a
   pipeline that only runs when you run CloudSDD is arguably not a
   pipeline. Proposed: no webhook in this RFC, revisited when there is a
   reason beyond symmetry with other CI tools.
2. ~~**ACR retention needs a Premium registry.**~~ **Decided — see §2.8.1.**
   The question was argued with, as it asked to be, and its premise did
   not survive: ACR's native retention policy is age-based and reaps only
   untagged manifests, so Premium would have bought a policy that deletes
   close to nothing. Azure keeps a Basic registry and honours `retain`
   with a timer-triggered `acr purge --keep` task. §2.8.1 also records the
   second ACR gap the original question did not notice — there is no
   registry-wide immutable-tags setting, so §2.4.1's guarantee is weaker
   on Azure than on the other two clouds.
3. **`agnostic` resolution for a pipeline and its service.** Nothing
   currently stops a Specification resolving a pipeline to GCP and the
   service that consumes it to AWS, which would work — a registry is
   reachable across clouds — while being almost certainly not what anyone
   meant, and while paying egress for every pull. Proposed: a service and
   its pipeline must resolve to the same provider, refused otherwise.
4. **Build caching.** None is configured, so every build is cold. That is
   correct for reproducibility and slow for iteration. Deferred rather
   than decided, because a cache is a persistent artifact with its own
   poisoning story.
5. **What happens on a rebuild with no source change?** The revision
   resolves to the same commit and the digest is unchanged, so the plan
   should show no change to the service. Worth an explicit test rather
   than an assumption, because the obvious implementation rebuilds and
   produces a byte-different image from a non-reproducible layer.
