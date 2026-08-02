# RFC 017: Container Service

- **Status:** Implemented (2026-08-02)
- **Author:** Claude (Senior Staff Cloud Platform Engineer, AI-assisted)
- **Date:** 2026-08-01
- **Depends on:** [RFC 012](012-environment-power-scheduling.md),
  [RFC 013](013-compute-instance.md),
  [RFC 014](014-agnostic-provider-resolution.md),
  [RFC 016](016-environment-network.md) — landed 2026-08-01, no longer blocking
- **Amended:** 2026-08-02 — §2.7 adds network egress, which RFC 016 §7.1
  had left open and assigned here. It moves to the front of the rollout
  because it repairs `compute_instance`, which RFC 016 shipped without a
  working path to Session Manager.
- **Amended:** 2026-08-02, during step 3 — §2.3.1 adds the `domain`
  property. §2.6 promised HTTPS through an ACM certificate with no field
  to name its subject, and ACM will not issue one for an ALB's own DNS
  name, so `public: true` on AWS was undeliverable as specified.
- **Amended:** 2026-08-02, during step 4 — §2.6 records the Container Apps
  subnet requirement, which the RFC 016 address plan does not satisfy
  without naming a workload profile.
- **Amended:** 2026-08-02, during step 5 — §2.5 records how "off" is
  actually spelled on each platform. Neither of the two mechanisms it
  originally named survived contact: Cloud Run's zero ceiling means
  *unset*, and the Azure runbook cannot PATCH a body.

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
about NAT. §2.7 records how that settlement went — not "this is the
resource type that actually needs egress", as this paragraph first
claimed, but "a resource type RFC 016 already shipped has needed it all
along".

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
    "public": true,
    "domain": "api.acme.example"
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
| `domain` | string | — | The hostname a public service is served on. Required when `public` is true on AWS; see §2.3.1 |

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

#### 2.3.1 `domain`, added during step 3

This section originally assumed every platform could hand back an HTTPS
endpoint the way Cloud Run does. Step 2 confirmed that for GCP: a Cloud
Run service is served on `*.run.app` with a Google-managed certificate,
so "public means HTTPS" is true with nothing configured.

**AWS cannot do this.** §2.6 says "ALB, HTTPS, ACM certificate" and the
property table had no field to name a certificate's subject. ACM will not
issue a certificate for an ALB's own `*.elb.amazonaws.com` name, and there
is no AWS equivalent of `*.run.app`. So on AWS, `public: true` under the
original properties could only have been delivered as a plain HTTP
listener — the one thing §2.3 refuses — or not at all.

So the schema gains **`domain`**: the hostname a public service is served
on.

- **Required when `public` is true on AWS.** Absent, it is a `Validate`
  error naming the reason, not a silent downgrade to HTTP.
- **Refused when `public` is false**, on every provider. A hostname on a
  service nothing outside can reach is a property the user asked for and
  will not get, which RFC 011 §2.1 rejects as a class.
- **Optional on GCP and Azure**, where the platform already serves the
  service on a managed-certificate endpoint of its own — `*.run.app` and
  `*.azurecontainerapps.io`. Absence means that endpoint; presence means a
  Cloud Run domain mapping or a Container Apps custom domain, both with
  managed certificates.

The domain must live in a hosted zone in the target account — **Route 53**
on AWS, **Azure DNS** on Azure. That is a real constraint and it is stated
rather than discovered:
certificate issuance needs DNS validation records, and the alternative —
emitting the records and asking the user to create them by hand — would
make `apply` block indefinitely on an action CloudSDD cannot see happen.
A domain outside that zone is an error naming the zone that was looked for.

CloudSDD creates, in the user's zone, the records each platform needs: on
AWS the ACM validation record and an alias record pointing the domain at
the load balancer; on Azure a CNAME to the app's ingress FQDN and the
`asuid.`-prefixed TXT record Azure issues its managed certificate against.
Without the resolving record the certificate is valid and the hostname
points nowhere, which is a deployment that reports success and serves
nothing.

The alternative considered and rejected was CloudFront in front of an
internal ALB: `*.cloudfront.net` carries a default certificate, so it
would deliver HTTPS with no domain and no hosted zone, matching Cloud
Run's ergonomics. It was rejected because it silently changes what the
user deployed — a CDN with its own caching semantics, its own regional
behaviour and its own bill, in front of an API they asked to be
reachable. A property they must supply is more honest than infrastructure
they did not ask for.

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
  schedule is largely redundant. ~~It is honoured by setting max instances
  to zero, which is the only way to guarantee nothing runs.~~ **Settled
  during step 2, and not the way this line assumed.** Cloud Run's
  `maxInstanceCount` is an `int32` the API reads as *unset* when it is
  zero, so a service asking for no instances would be created with
  Google's own default ceiling instead — the opposite of what was written.
  Setting it to zero does not stop the service; it uncaps it.

  So `replicas: 0` is a `Validate` error on GCP
  (`ErrZeroReplicasUnsupported`), per RFC 012 §1.3: a rule a provider
  cannot express is refused, never dropped. Nothing is lost in practice —
  Cloud Run's floor is already zero between requests, so what is refused
  is a way of spelling "and never scale up", not the saving itself. Step 5
  decides how an *inherited* schedule composes with that, which is a
  different question from an explicit zero in the Specification.
- **Azure Container Apps** — ~~min and max replicas both to zero.~~
  **Settled during step 5, through a better mechanism.** Both bounds to
  zero would need a PATCH with a body, and the RFC 012 §4.3 runbook
  deliberately cannot do that: it POSTs an action and interpolates nothing
  from the Specification into script text. Container Apps turned out to
  offer `start` and `stop` actions of its own, which express the same
  intent through the machinery the databases already use — a stopped app
  runs no replicas and bills for none, with no `deallocate` distinction to
  get wrong.

The RFC 012 §1.3 rule holds unchanged: a rule a provider cannot express is
a `Validate` error, never a dropped rule. Cloud Run is where it bites, and
step 5 drew the line the rule implies:

- an **explicit** schedule on a Cloud Run service is a request the user
  wrote and will not get, so it is refused (`ErrCloudRunNotSchedulable`);
- an **inherited** one is merely inapplicable, so it is accepted, nothing
  is declared, and the plan reports the service as staying up.

That distinction already existed in RFC 012 for types that support no
schedule at all; what step 5 added is that it is now **provider-aware**.
`ResourceType.SupportsScheduleOn(Provider)` carries the one exception, and
it lives beside `SupportsSchedule` rather than only inside the GCP
provider, because the Engine and the provider both have to agree on it and
two copies of an answer drift (RFC 011 §1.1H).

### 2.6 Per-provider mapping

| | AWS | GCP | Azure |
|---|---|---|---|
| Service | ECS on Fargate | Cloud Run | Container Apps, consumption workload profile (see below) |
| Ingress when `public` | ALB, HTTPS, ACM certificate | Built-in HTTPS endpoint | Managed ingress, HTTPS |
| Ingress when private | Internal ALB in the scope's subnets | Ingress restricted to internal traffic | Internal ingress |
| Identity | Task role with no policies attached | Dedicated service account, no roles | Managed identity, no assignments |
| Egress for image pull | NAT gateway, one per scope (§2.7) | Cloud NAT on the scope router (§2.7) | NAT gateway on the scope subnet (§2.7) |

**Azure needs a workload profile, and it is not a performance choice.**
A Container Apps environment is injected into a delegated subnet, and a
*consumption-only* environment requires that subnet to be at least a `/23`.
The RFC 016 address plan gives each scope a `/20` carved into `/24`s, so a
consumption-only environment does not fit the plan at all. Naming a
workload profile lowers the requirement to a `/27`, which a `/24` satisfies
with room to spare. The alternative — widening every scope's subnets to
`/23` for a resource type most scopes will not hold — would re-address
every network already deployed to accommodate one that is not.

The identity row is the one to notice: on every provider the service gets
an identity of its own with **nothing attached**, following RFC 013's
choice to give a VM no standing credential. A container that needs to call
a cloud API will need a way to say so, and that is the same future RFC as
configuration injection.

### 2.7 Egress, which is already owed to a resource type that exists

RFC 016 §7.1 left NAT open and named this RFC as the one that would settle
it, on the reasoning that a container image pull is the first thing that
genuinely cannot work without egress. That reasoning was half right. The
half it missed is that **`compute_instance` is already broken by its
absence**, and has been since RFC 016 landed.

`internal/provider/aws/network.go` argues in a comment that "a database
and a VM reached through Session Manager both work without egress". A
database does. A VM does not: the SSM agent is a client, and it has to
*reach* `ssm`, `ssmmessages` and `ec2messages` before any session can be
opened. Without a NAT gateway that requires interface VPC endpoints, which
the scope network does not declare either. So today an RFC 013 instance
lands in a subnet where it can neither update a package nor register with
Session Manager — it boots, and nothing can talk to it.

That makes egress a defect to repair rather than a feature to add, which
is why it moves to the front of §6's rollout instead of arriving with AWS.

**The shape, per provider:**

- **AWS.** A new `public` subnet tier holding an internet gateway and a
  **single NAT gateway for the whole scope**, with the private subnets
  routing `0.0.0.0/0` through it. §2.4 of RFC 016 carves `/24`s out of the
  scope's `/20` with room for sixteen and `tagSubnetTier` already exists
  to tell tiers apart, so this is a new tier in a layout built to receive
  one — not a renumbering of anything deployed.
- **GCP.** A Cloud Router with a Cloud NAT on the scope's network. No
  public subnet: GCP expresses egress as a property of the router rather
  than of the subnet, so instances keep their private-only addressing.
- **Azure.** A NAT gateway with a public IP, associated with the scope
  subnet. Same effect, expressed at the subnet.

**One NAT, not one per availability zone.** RFC 016 §7.1 priced a NAT
gateway at roughly $32/month per AZ and called that a real cost to impose
by default on a tool whose other headline feature is switching things off
at night. Per-AZ NAT buys two things: survival of a single-AZ outage, and
no cross-AZ data charge on egress. Neither is worth doubling the standing
cost of every environment CloudSDD creates, for traffic that is dominated
by package updates and image pulls. A user who needs zonal-failure-proof
egress is past what this tool decides for them.

**Unconditional, provisioned with the network.** The alternative — create
the NAT lazily, when a resource that needs egress first appears — is
cheaper for a scope holding only databases, and is rejected. It makes the
network's shape depend on the order resources are applied in, it gives
`ReapNetworks` a second question to answer beyond "is this scope empty",
and it reintroduces exactly the failure this section exists to fix: a
network that is quietly missing a route out until something discovers it
at runtime. RFC 016 §1 argued that a network which is secure but
unreachable is a defect, not a hardening; a network that cannot reach a
package mirror is the same argument pointed outward.

**What egress does not become.** A route out is not a route in. The public
subnet tier exists to hold a NAT gateway and nothing else: no resource is
ever placed in it, `MapPublicIpOnLaunch` stays false everywhere, and
ingress remains what §2.3 defines — absent unless `public` is set, and
HTTPS through a managed load balancer when it is.

This resolves RFC 016 §7.1, which is updated to point here.

## 3. Impacted JSON Schema

No new `ResourceType` — it has been in the enum since RFC 001; this makes
it real. New:

- `ContainerServiceProperties` in `docs/openapi.yaml` (`image`, `port`,
  `size`, `replicas`, `public`, `domain` — the last added during step 3,
  §2.3.1);
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
| Egress turns into ingress | The public tier holds the NAT gateway and nothing else; no resource is placed in it and no subnet assigns a public address on launch (§2.7) |
| A workload reaches the internet unnoticed | Egress is NAT'd through one gateway per scope, so it leaves from one address that an account-level control can see and constrain |

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
8. **Egress** (§2.7), asserted against the declared graph rather than a
   live cloud: the scope network declares exactly one NAT gateway
   regardless of `subnetCount`; the private subnets' route table carries
   a default route through it; the public tier holds the NAT and nothing
   else; and no subnet on any provider sets a public address on launch.
   The AWS test also asserts the public subnet's own `/24` comes out of
   the scope's `/20` and overlaps none of the private ones, since
   `subnetBlock` is now asked for one more block than before.

## 6. Rollout

Additive: no existing resource changes shape, because nothing can have
been deployed as a `container_service`. The one behavioural change to
something that exists is `policies.allowed_registries`, a new optional
field that defaults to absent and constrains nothing when unset.

1. **Egress on all three providers (§2.7)**, plus
   `internal/provider/container` with the shared properties and image
   rules and the Engine-level `allowed_registries` enforcement.
   Egress leads because it repairs `compute_instance`, which is deployed
   today, rather than because a container needs it — and shipping it
   first means the repair is not hostage to the rest of this RFC.
2. One provider end to end — GCP first, because Cloud Run needs the least
   surrounding infrastructure and will surface design errors soonest.
3. AWS, which needs the most: ALB, target group, task definition.
4. Azure.
5. RFC 012 composition across all three.
6. `cli.md`, `architecture.md`, `openapi.yaml`, and the system prompt.

At step 6 the schema has no unimplemented `ResourceType` left, which is
the first time that will have been true.

**Status: complete, 2026-08-02.** Four of this RFC's own proposals did not
survive contact with the platforms, and each is recorded where it was
made rather than quietly corrected: §2.7 (egress was owed to a resource
type RFC 016 had already shipped), §2.3.1 (`domain`, because ACM cannot
certify a load balancer's own name), §2.6 (the Container Apps workload
profile, an addressing constraint rather than a performance choice), and
§2.5 twice over (Cloud Run reads a zero ceiling as *unset*; the Azure
runbook cannot PATCH a body).

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
