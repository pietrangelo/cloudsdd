# RFC 017: Container Service

- **Status:** Proposed
- **Author:** Claude (Senior Staff Cloud Platform Engineer, AI-assisted)
- **Date:** 2026-08-01
- **Depends on:** [RFC 012](012-environment-power-scheduling.md),
  [RFC 013](013-compute-instance.md),
  [RFC 014](014-agnostic-provider-resolution.md),
  [RFC 016](016-environment-network.md) — **blocking**

## 1. Problem

`container_service` is the last `ResourceType` in the v1.0 schema that no
provider implements. It is not passively missing:

- `spec.ResourceType` accepts it, and the `oneof` validator tag lists it;
- `internal/nlp/prompt.go` tells the model it is a valid `type`;
- `ResourceType.SupportsSchedule()` returns **true** for it, so the engine
  will happily compile a power schedule for a resource type that cannot be
  created;
- `docs/openapi.yaml` publishes it in the type enum.

So a user who asks for a containerised service gets a Specification that
validates, a schedule that compiles, a plan that is rendered — and then:

```
Error: engine: resource "api": aws: unsupported resource type: "container_service"
```

This is exactly the defect RFC 013 was written to close for
`compute_instance`, left standing for the one type RFC 013 did not cover:
**a published contract advertising a capability nothing delivers**. With
RFC 014 it acquired a second edge — an `agnostic` container service now
fails with `ErrNoCandidateProvider` quoting three providers' refusals,
which reads as "no cloud can do this" rather than "CloudSDD cannot".

## 2. Proposed Architecture

### 2.1 Why this blocks on RFC 016

A container service needs a network the way a VM does not. It must pull an
image from a registry, which means egress; and it exists to be reached,
which means an ingress path that is deliberate rather than inherited.

Implementing it against today's networking would mean AWS Fargate tasks in
the account's default VPC — §1's second defect in RFC 016, on a resource
type whose entire purpose is to serve traffic. This RFC therefore does not
start until RFC 016 lands, and it settles RFC 016 §7.1's open question
about NAT, because this is the resource type that actually needs egress.

### 2.2 Properties

Cloud-agnostic, in `internal/provider/container`, shared by all three
providers for the reason RFC 011 §1.1H recorded and RFC 013 followed:

```json
{
  "id": "api",
  "type": "container_service",
  "provider": "agnostic",
  "scope": { "region": "europe-west1", "environment": "prod" },
  "properties": {
    "image": "ghcr.io/acme/api@sha256:9f2c…",
    "port": 8080,
    "size": "small",
    "replicas": 2,
    "public": true
  }
}
```

| Property | Type | Default | Notes |
|---|---|---|---|
| `image` | string, required | — | Registry reference; see §2.4 |
| `port` | int, required | — | The port the container listens on, 1-65535 |
| `size` | enum | `small` | `small`/`medium`/`large`, reusing RFC 013's vocabulary, mapped to each platform's CPU/memory pairs |
| `replicas` | int | `1` | 0-10. Zero is legal and means "deployed but not running" |
| `public` | bool | `false` | §2.3 |

Deliberately absent: environment variables, secrets, volumes, custom
commands, health-check paths. Each is a feature, and `env` in particular
is where credentials get pasted — the denylist in
`internal/provider/decode` rejects credential-shaped *keys*, and a map of
free-form values would drive straight through it. Configuration injection
deserves its own RFC with a secrets story, not a `map[string]string` on
this one.

### 2.3 `public` is a property, and the default is still false

A container service exists to serve traffic, so RFC 016's "secure but
unreachable is a defect" argument applies with more force here than
anywhere. But defaulting to public would make the one resource type that
runs arbitrary user code the one type exposed to the internet by default,
which is not a trade this project should make.

So: `public: false` by default, and when set:

- ingress is **HTTPS only**, through the platform's managed load balancer
  with a managed certificate — never a raw port opened to `0.0.0.0/0`;
- the container's own port is reachable only from that ingress, never
  directly;
- plain HTTP redirects, rather than being served.

The user asks for "public" and gets TLS, because asking for TLS separately
is the kind of thing the mandate says they should not have to do. When
`public` is false the service is reachable only from its scope's network
(RFC 016 §2.4), which is what makes an internal API expressible.

### 2.4 The image is the supply chain

RFC 013 §4 established that an AMI lookup filters on owner ID, not just a
name pattern, because a name-only filter lets any account publishing a
matching public image be selected. The container equivalent is worse: a
mutable tag means the artifact can change after approval, without any
change to the Specification the user reviewed.

Two rules:

- **A digest is required unless the registry is allow-listed.** An
  `image` carrying `@sha256:…` is accepted from anywhere. A tag is
  accepted only from a registry named in `policies.allowed_registries`.
  Bare `:latest` is refused outright, with or without an allowlist:
  nothing that reads "deploy whatever is newest, forever" belongs in a
  reviewed artifact.
- **`policies.allowed_registries`** joins `allowed_regions` as a
  Specification-level policy, enforced in the Engine for every provider
  (RFC 011 §2.3's lift: a check living only in providers is a check the
  next provider forgets).

This is the one place this RFC is stricter than the platforms are. It is
also the only property whose value decides what code runs.

### 2.5 Scheduling means scaling to zero

`SupportsSchedule()` already returns true for this type, so RFC 012 must
compose rather than be bolted on. A container service has no power state;
it has a replica count, and the equivalent of "off" is zero:

- **AWS ECS/Fargate** — the schedule sets the service's desired count
  between `replicas` and `0`, through the same EventBridge Scheduler
  universal-target mechanism RFC 012 §4.1 uses for RDS and EC2, so no code
  artifact is deployed.
- **GCP Cloud Run** — scales to zero natively and bills per request, so a
  schedule is largely redundant. It is honoured by setting max instances
  to zero, which is the only way to guarantee nothing runs.
- **Azure Container Apps** — min and max replicas both to zero.

The RFC 012 §1.3 rule holds unchanged: a rule a provider cannot express is
a `Validate` error, never a dropped rule.

### 2.6 Per-provider mapping

| | AWS | GCP | Azure |
|---|---|---|---|
| Service | ECS on Fargate | Cloud Run | Container Apps |
| Ingress when `public` | ALB, HTTPS, ACM certificate | Built-in HTTPS endpoint | Managed ingress, HTTPS |
| Ingress when private | Internal ALB in the scope's subnets | Ingress restricted to internal traffic | Internal ingress |
| Identity | Task role with no policies attached | Dedicated service account, no roles | Managed identity, no assignments |
| Egress for image pull | NAT or VPC endpoints — RFC 016 §7.1 | Default | Default |

The identity row is the one to notice: on every provider the service gets
an identity of its own with **nothing attached**, following RFC 013's
choice to give a VM no standing credential. A container that needs to call
a cloud API will need a way to say so, and that is the same future RFC as
configuration injection.

## 3. Impacted JSON Schema

No new `ResourceType` — it has been in the enum since RFC 001; this makes
it real. New:

- `ContainerServiceProperties` in `docs/openapi.yaml` (`image`, `port`,
  `size`, `replicas`, `public`);
- `policies.allowed_registries`, a string array, `omitempty`, capped at 10
  entries in line with the RFC 005 §3 bounds on policy lists;
- `internal/nlp/prompt.go` gains the property list and, importantly, the
  instruction **not to invent an image**: a hallucinated `image` is a
  hallucinated artifact, and the model must carry through what the user
  named or fail.

## 4. Security Considerations

| Threat | Mitigation |
|---|---|
| Mutable image tag changes the artifact after review | Digest required unless the registry is allow-listed; `:latest` refused outright (§2.4) |
| Image pulled from an attacker-controlled registry | `policies.allowed_registries`, enforced in the Engine so no provider can omit it |
| The one type running arbitrary code is public by default | `public` defaults to false; when true, ingress is HTTPS-only through a managed load balancer, never a raw open port |
| Plaintext transport | HTTP redirects to HTTPS; no listener serves plain HTTP |
| Standing cloud credentials in a workload | A dedicated identity with no policies, roles or assignments attached, per RFC 013's precedent |
| Credentials pasted into configuration | No `env` property exists to paste them into. Deferred to an RFC that can pair it with a secrets story (§2.2) |
| Container reachable directly, bypassing ingress | The container port is reachable only from the load balancer's security group / the platform's ingress |
| Unrestricted resource consumption | `replicas` capped at 10, `size` an enum of three — a Specification cannot request an unbounded fleet |
| A schedule that saves nothing | "Off" is zero replicas, not a stopped task that still bills (the RFC 013 §2.5 lesson, restated for containers) |

## 5. Testing Plan

Table-driven, `pulumi.WithMocks` for declaration, no Docker or cloud.

1. **Property decoding**: required `image` and `port`; `size` enum;
   `replicas` bounds including the legal zero; `public` defaulting false;
   unknown properties rejected.
2. **Image rules**, the heart of this RFC: a digest accepted from any
   registry; a tag rejected without an allowlist; a tag accepted from an
   allow-listed registry; `:latest` rejected in every combination,
   including from an allow-listed registry.
3. **`allowed_registries` enforced at the Engine**, not only in providers
   — the RFC 011 §2.3 test shape, with a mock provider that would accept
   anything.
4. **Ingress**, per provider: `public: true` declares an HTTPS listener
   and a certificate and no plain-HTTP listener; `public: false` declares
   an internal ingress and no public address.
5. **Identity**: the declared role/service account/managed identity has no
   policy, binding or assignment attached.
6. **Scheduling**: a scheduled service declares rules toggling replicas
   between `replicas` and 0, and an unscheduled one declares no scheduling
   resources at all.
7. **Agnostic resolution** (RFC 014): a `container_service` naming a
   region resolves to that region's provider, and resolution does not
   provision anything on the other two.

## 6. Rollout

Additive: no existing resource changes shape, because nothing can have
been deployed as a `container_service`. The one behavioural change to
something that exists is `policies.allowed_registries`, a new optional
field that defaults to absent and constrains nothing when unset.

1. `internal/provider/container` with the shared properties and image
   rules, plus the Engine-level `allowed_registries` enforcement.
2. One provider end to end — GCP first, because Cloud Run needs the least
   surrounding infrastructure and will surface design errors soonest.
3. AWS, which needs the most: ALB, target group, task definition, and the
   egress decision RFC 016 §7.1 left open.
4. Azure.
5. RFC 012 composition across all three.
6. `cli.md`, `architecture.md`, `openapi.yaml`, and the system prompt.

At step 6 the schema has no unimplemented `ResourceType` left, which is
the first time that will have been true.

## 7. Open Questions

1. **NAT gateway, inherited from RFC 016 §7.1.** Fargate cannot pull an
   image without egress. VPC endpoints for ECR and S3 cost less than a NAT
   gateway and cover the ECR case exactly, at the price of only working
   for ECR — a public image from GHCR still needs NAT. Proposed: VPC
   endpoints by default, NAT only when the image is not in ECR, decided by
   inspecting the registry we already parse in §2.4.
2. **Cloud Run is regional and serverless; ECS Fargate is neither.**
   "Small" on Cloud Run is a request-scaled container, on Fargate a
   long-running task with a fixed cost. The cloud-agnostic promise is
   thinner here than for a VM or a database, and RFC 014 will happily
   resolve `agnostic` between them. Worth stating in `cli.md` rather than
   pretending the three are equivalent.
3. **Custom domains** are absent: `public` gives you the platform's
   generated hostname. Bringing a domain means DNS and certificate
   validation, which is its own RFC.
4. **Should `replicas: 0` be legal?** It is, above, meaning "deployed but
   not running", which composes with scheduling. The alternative reading
   is that it is a mistake worth rejecting. Proposed: legal, because RFC
   012 already makes zero a state the system produces on purpose.

## 8. Implementation notes

- `SupportsSchedule()` already returns true for `container_service`, so no
  change is needed there — but the engine's `ErrResourceNotSchedulable`
  path should be re-tested once the type is real, since it has never been
  exercised with a schedulable type that no provider implemented.
- The `oneof` validator tag on `Resource.Type` already lists
  `container_service`; nothing in `internal/spec` changes.
- `policies.allowed_registries` follows `allowed_regions`: a
  `provider.ValidateRegistryAllowed` helper next to
  `ValidateRegionAllowed`, called from `DefaultEngine.Validate` and from
  each provider as defence in depth.
- Image parsing must handle registry, repository, tag and digest without a
  dependency: `name@sha256:…`, `registry/name:tag`, and the implicit
  Docker Hub registry when none is given. The implicit case should be
  treated as a registry named `docker.io` for allowlist purposes, so that
  an allowlist cannot be bypassed by omitting the host.

---

**Do you approve this RFC?**
