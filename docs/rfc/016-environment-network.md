# RFC 016: The Environment Network

- **Status:** Approved (2026-08-01), implemented
- **Author:** Claude (Senior Staff Cloud Platform Engineer, AI-assisted)
- **Date:** 2026-08-01
- **Depends on:** [RFC 005](005-account-environment-region-scoping.md),
  [RFC 007](007-aws-relational-database.md),
  [RFC 008](008-gcp-azure-providers.md),
  [RFC 013](013-compute-instance.md),
  [RFC 015](015-azure-database-parity.md)

## 1. Problem

CloudSDD advertises "private by default" and delivers three different
things, only one of which produces infrastructure anything can connect to:

| Provider | What "private" means today | Reachable from |
|---|---|---|
| AWS `relational_database` | `PubliclyAccessible: false` in the **default VPC** | Anything in the default VPC — including resources CloudSDD never created |
| AWS `compute_instance` | Security group with no ingress, in the default VPC | Nothing inbound; egress via the default VPC |
| GCP `relational_database` | `Ipv4Enabled: false`, no `PrivateNetwork` | **Nothing.** A private IP requires a VPC with private services access |
| GCP `compute_instance` | `default` network, explicit deny-ingress at priority 0 | Nothing inbound |
| Azure (RFC 015) | A VNet, subnet and private DNS zone **per resource** | Only what is in that one-resource VNet — i.e. nothing |

Two distinct defects hide in that table.

**A database no application can reach is not a feature.** RFC 015 §1.1
named this and deferred it. On GCP and Azure the posture is secure in the
way an unplugged server is secure: `cloudsdd deploy "a postgres database
and a VM that uses it"` produces two resources that cannot exchange a
packet, and nothing in the output says so. The user is not told they have
bought an unusable database.

**AWS's default VPC is the opposite failure.** It is reachable — by
everything else in the account. `Scope.Environment` is documented as a
boundary and `Scope.Sealed` defaults to `true`, "sealed unless otherwise
specified" (RFC 005 §2.5). At the network layer neither is true: a `dev`
database and a `prod` database land in the same default VPC, alongside
whatever else the account contains, and the `Sealed` flag that is supposed
to mean isolation is consulted by exactly one ResourceType
(`cross_account_role`) and ignored by every network decision.

So the system is simultaneously too closed to use and too open to trust,
and which one you get depends on the cloud.

### 1.1 Why this could not be fixed inside the earlier RFCs

Every network primitive so far has been declared *inside a single
resource's* Pulumi program, because that is the only place a provider
could put it: stack identity is `(Account, Environment, Region,
Resource.ID)` (RFC 005 §2.6), and a resource's program cannot declare
something a *different* stack also needs. RFC 015 built Azure a VNet the
only way it could — one per database — which is why it produced isolation
without connectivity.

A shared network is therefore not another resource inside a program. It is
a thing that outlives and precedes the resources in it, and this RFC is
mostly about where such a thing lives.

## 2. Proposed Architecture

### 2.1 The unit of sharing is `(Account, Environment, Region)`

One network per account, per environment, per region. Not per resource,
which is what we have; not per account, which would put `dev` and `prod`
on the same wire.

This is the existing stack scope minus `Resource.ID`, and that is not a
coincidence: RFC 005 chose those three dimensions because they are what
separates one deployment context from another, and the network is the
physical expression of exactly that separation. `Scope.Environment`
finally means something enforceable, and `Scope.Sealed` acquires the
meaning its name has always implied — §2.5.

**Accounts never share a network. This is an invariant, not a default.**
A `DeploymentTarget` is a different account reached through different
credentials (RFC 004), and its networks are built with those credentials,
in that account, from that account's own address block (§2.4.1). There is no
configuration that connects two accounts' networks, no shared DNS zone
spanning them, and no CloudSDD-created route between them — including for
`sealed: false` scopes, where §2.5's eligibility to be peered means
*within one account only*.

The reasoning is the same one RFC 004 §2.1 used to keep `DeploymentTarget`
out of the Specification: the account boundary is the strongest one the
cloud gives us, and a tool that quietly spans it has removed a control the
operator was relying on without being asked. A cross-account path is a
thing an operator builds deliberately, with peering or a transit gateway
they own; it is not something a deployment tool should be able to create
as a side effect of two resources sharing an `environment` label.

Concretely, this means the account is the first segment of the network
stack's name, the first input to address derivation, and a hard partition
in the conflict check — three separate mechanisms that would each have to
fail before two accounts could touch.

### 2.2 The network is its own stack, and the Engine sequences it

The network gets a Pulumi stack of its own, named by the scope it serves:

```
prod::live::eu-central-1::app-db      # resource stack, unchanged
prod::live::eu-central-1              # network stack, new
```

The name is what `stackNameFor` already produces for an empty
`Resource.ID` — the network is the scope itself rather than something in
it. Resource programs discover it by **tag**, not through a Pulumi
`StackReference`: see §7.2, which records why the mechanism this section
originally proposed was replaced during implementation.

Ordering is the hard part, and it belongs to the Engine rather than to the
providers. `CloudProvider` gains one method:

```go
// EnsureNetwork idempotently provisions the shared network for a scope,
// returning nil when the provider has nothing to share.
EnsureNetwork(ctx context.Context, s spec.Scope, p spec.Policies) error
```

The Engine calls it once per distinct `(Account, Environment, Region)` in
a Specification, before applying any resource in that scope. Three
consequences worth stating:

- **The Engine stays cloud-agnostic.** It knows "this scope needs its
  network to exist first". It does not know what a VPC is, which is the
  same line RFC 004 drew for `TargetProviderFactory` and RFC 014 for
  candidate resolution.
- **Ordering lives in one place.** CloudSDD has no dependency graph
  between resources (RFC 001, still deferred), and this RFC does not add
  one. It adds a single ordering constraint, expressed once, rather than
  each provider racing to create a VPC from inside three concurrent
  resource programs.
- **It is idempotent by construction.** A second call selects the existing
  stack and applies no change, so every resource in a scope may ask for
  the network without the Engine tracking who asked first.

The alternative designs were considered and rejected:

- *A `network` ResourceType the user declares.* It would work, and it
  contradicts the mandate: the user describes what they want and CloudSDD
  supplies the secure infrastructure. Making them write the plumbing they
  did not ask for is precisely what the tool exists to avoid.
- *Declaring the network in the first resource's stack.* Then destroying
  that resource destroys the network under the others' feet, and which
  resource is "first" depends on iteration order.
- *Importing whatever network already exists.* This is what AWS does today
  with the default VPC, and §1's second defect is the result.

### 2.3 What each provider builds

Same shape everywhere: a private network with no path in from the
internet, egress for patching, and room for the resource types CloudSDD
supports.

**AWS** — a VPC (address block per §2.4.1), private subnets in **at least two
availability zones** (RDS refuses a subnet group with fewer), a DB subnet
group, a NAT-less default route today, and no internet gateway unless a
`compute_instance` in the scope asked for `public_ip`. `relational_database`
moves out of the default VPC and gains `DbSubnetGroupName` and a security
group admitting the scope's own CIDR on the engine's port; `compute_instance`
attaches to the same VPC, keeping its no-ingress security group.

**GCP** — a VPC network and a regional subnet, plus the piece whose
absence is why GCP databases are unreachable today: a reserved global
address range and a `servicenetworking` connection (Private Services
Access), without which `Ipv4Enabled: false` yields an instance with no IP
at all. Cloud SQL then gets `PrivateNetwork` pointing at the VPC.
`compute_instance` moves off the `default` network, which also retires the
priority-0 deny rule that exists only to neutralise `default-allow-ssh`.

**Azure** — the VNet, subnet and private DNS zone RFC 015 built per
database, promoted to the scope. The delegated subnet stays per engine —
a subnet delegated to `Microsoft.DBforMySQL/flexibleServers` cannot host a
VM — so the scope's VNet carries a general subnet plus a delegated one per
engine actually in use.

RFC 015's per-resource network is not wasted work: it is this design at a
scope of one, and the change is where the objects hang, not what they are.

### 2.4 Connectivity is deliberately coarse

Within a scope, resources can reach each other on the ports their types
imply — a VM reaching the database's engine port, and nothing more
interesting. This RFC does **not** introduce per-resource network policy,
service discovery, or a way to say "only `app-vm` may reach `app-db`".

That restraint is deliberate. The gap being closed is *no path at all* on
two clouds and *everyone's path* on the third. A scope-wide private
network fixes both, and the finer-grained model needs the reference
between resources that CloudSDD does not yet have.

### 2.4.1 Address allocation, and why two scopes must never collide

Every network needs an address block, and handing every scope the same
`10.0.0.0/16` — which is what the first draft of this RFC proposed to do,
deferring the problem — is wrong for a reason worth being precise about.

While nothing is connected, overlapping ranges are harmless: two VPCs
using `10.0.0.0/16` in different accounts, or in the same account in
different regions, coexist perfectly well as long as no packet ever needs
to cross between them. The damage is deferred, not avoided. The day
somebody peers `dev` to a shared services VPC, or attaches a VPN, or
connects two regions, overlapping ranges make the operation **impossible**
— and the fix at that point is to rebuild the network, which means
rebuilding everything in it. A tool that allocates addresses carelessly is
writing a migration for its user to perform later.

So allocation is deterministic and collision-free by construction:

```
network CIDR = <base block> ⊃ derive(account, environment, region)
```

- **The base block** defaults to `10.0.0.0/8` and is configurable per
  account (§2.4.2), so an operator with an existing address plan can confine
  CloudSDD to the part of RFC 1918 space they have set aside for it.
- **`derive`** carves the base block into fixed-size slots — `/20` from a
  `/8`, giving 4096 of them — and picks one by hashing the scope tuple.
  The hash is stable across machines and releases, so the same scope
  always yields the same range no matter who runs `cloudsdd` or when. That
  property matters more than it looks: CloudSDD's state is a local file,
  so two engineers deploying two environments from two laptops cannot
  coordinate through it. Determinism is what lets them not need to.
- **A `/20` per scope** is 4096 addresses, split into subnets per
  availability zone with room to spare for every resource type CloudSDD
  supports. It is deliberately not larger: slots are the scarce resource,
  not addresses within a slot.

Hashing 4096 slots is not collision-*proof*, so the derivation is paired
with a check rather than trusted:

- The Engine knows every scope in the Specification and the ledger records
  every scope ever deployed, so before applying anything it derives the
  range for each and **refuses to proceed if two distinct scopes in the
  same account derive the same one**, naming both and the override that
  resolves it.
- Refusing is the RFC 014 stance, applied to addresses: an ambiguity the
  system cannot resolve correctly is reported, never broken by a guess. A
  silently overlapping VPC is the kind of defect that surfaces months
  later, during an operation that cannot then be completed.

Accounts are partitioned *before* any of this: the account is an input to
`derive`, so two accounts' scopes are compared for collision only against
their own account's, never against each other's. Two accounts reusing the
same range is not a collision — it is the normal, correct outcome of
§2.1's invariant, and flagging it would be noise.

### 2.4.2 The explicit override

Derivation covers the common case; it cannot cover an operator whose
corporate address plan says "this account gets `172.20.0.0/14` and
nothing else". So:

```json
"policies": {
  "network": {
    "base_cidr": "172.20.0.0/14",
    "scopes": {
      "prod::live::eu-central-1": "172.20.16.0/20"
    }
  }
}
```

`base_cidr` confines derivation; the optional `scopes` map pins an
individual scope, which is also the escape hatch when the collision check
refuses. An explicit range is validated for being RFC 1918, for fitting
inside `base_cidr`, and for not overlapping any other range in the same
account — the same check derivation is held to, since a hand-written range
is at least as likely to collide as a computed one.

### 2.5 `Sealed` becomes enforceable

`Scope.Sealed` has defaulted to `true` since RFC 005 and been consulted by
one ResourceType. It now describes the network:

- **`sealed: true`** (default) — the scope's network has no route to any
  other scope's network, and none to the internet beyond egress. A sealed
  scope may not be peered.
- **`sealed: false`** — the scope is *eligible* to be connected to
  another. This RFC provisions no peering; it makes the declaration
  meaningful and refuses the operation on a sealed scope, so that the flag
  is load-bearing before the feature that reads it arrives.

Making `Sealed` mean nothing at the network layer while it is named
"sealed" is the documentation defect RFC 015 was written to fix, one layer
down.

### 2.6 Destroying a network nobody is using

A shared network must not be destroyed while resources sit in it, and must
not be left behind forever either. The ledger already answers this: it is
keyed by `(account, environment, region, id)` (RFC 011 §2.7), so after a
`destroy` the Engine can ask whether any entry remains in the scope, and
tear the network stack down only when none does.

This makes the ledger load-bearing for correctness rather than only for
translation context, which is a real change in its status and is called
out in §4 as such.

## 3. Impacted JSON Schema

Mostly a reinterpretation of existing fields:

| Field | Before | After |
|---|---|---|
| `scope.environment` | A label used for stack naming and ledger keying | Also the network boundary: resources sharing it, *in one account*, share a network |
| `scope.sealed` | Consulted only by `cross_account_role` | The network's isolation posture (§2.5) |
| `scope.region` | Where the resource is created | Also which network it joins |

A user who never mentions networking gets a private, non-overlapping
network, which is what "the provider enforces best practice without being
asked" means (CLAUDE.md). Everything below is optional and exists for the
operator who has an address plan to respect.

One new policy object, `policies.network`:

```yaml
network:
  base_cidr: string          # optional; RFC 1918 block, default 10.0.0.0/8
  scopes:                    # optional; explicit per-scope pins
    "<account>::<environment>::<region>": string
```

| Field | Validation |
|---|---|
| `base_cidr` | RFC 1918 (`10/8`, `172.16/12`, `192.168/16`), prefix ≤ `/20` so at least one slot fits |
| `scopes` | Max 32 entries; each key a scope tuple in `stackNameFor` form; each value RFC 1918, inside `base_cidr`, non-overlapping with every other range in the same account |

`docs/openapi.yaml` gains `NetworkPolicy` and the reinterpretation in the
`Scope` schema's descriptions.

## 4. Security Considerations

| Threat | Mitigation |
|---|---|
| Environments sharing a network | One network per `(Account, Environment, Region)`. `dev` cannot reach `prod` because there is no route, not because a rule says so |
| Inheriting whatever the account already has | CloudSDD creates its own VPC and stops using the AWS default VPC, whose contents it does not control and did not audit |
| Internet exposure | No internet gateway unless a `compute_instance` in the scope requested `public_ip`; databases are never in a public subnet |
| `Sealed` meaning nothing | §2.5 gives it network semantics and refuses peering on a sealed scope |
| A network destroyed under live resources | Teardown is gated on the ledger showing no remaining entries in the scope (§2.6) |
| Over-broad intra-scope reachability | Accepted and documented (§2.4): scope-wide, port-limited by resource type. Finer policy needs inter-resource references and its own RFC |
| A stale ledger stranding or orphaning a network | **New risk.** The ledger becomes load-bearing for a destroy decision, not merely for translation context. A ledger deleted by hand leaves the network stack orphaned — recoverable, since the stack still exists and is named by its scope, but it needs saying |
| Cross-account reachability | **Invariant, not a default** (§2.1). Accounts never share a network: the account is a segment of the network stack's name, an input to address derivation, and a partition in the conflict check. `sealed: false` makes a scope eligible for peering *within its own account* only; no CloudSDD-created route crosses an account boundary, ever |
| Address collision making a future connection impossible | Ranges are derived deterministically per scope from a configurable base block, and the Engine refuses to apply when two distinct scopes in one account resolve to the same range (§2.4.1). Overlapping VPCs are harmless until the day somebody peers them, at which point the only fix is rebuilding the network and everything in it |
| Two accounts deriving the same range | Not a collision, and deliberately not flagged: it is the correct outcome of §2.1. Comparing across accounts would be noise, and silencing noise is how a real warning gets ignored |
| An operator's existing address plan silently violated | `policies.network.base_cidr` confines derivation to the block the operator set aside; explicit per-scope pins are validated for RFC 1918, containment and non-overlap, i.e. held to the same standard as a derived range |

## 5. Testing Plan

1. **Scope→stack naming**: the network stack for a scope is
   `stackNameFor(account, environment, region, "")`, and never collides
   with a resource stack. Table-driven over the RFC 005 scope matrix.
2. **Engine sequencing**: `EnsureNetwork` is called once per distinct
   scope, before any `Apply` in it, and not once per resource. A mock
   provider records call order.
3. **Idempotence**: two Applies in one scope produce one `EnsureNetwork`
   effect.
4. **Provider declaration**, per cloud, through `pulumi.WithMocks`: AWS
   declares a VPC with subnets in ≥2 AZs and a DB subnet group; GCP
   declares the servicenetworking connection without which a private IP
   cannot exist; Azure declares one VNet carrying a delegated subnet per
   engine in use.
5. **Reachability wiring**: the database is declared *in* the scope's
   network — `DbSubnetGroupName` on AWS, `PrivateNetwork` on GCP,
   `DelegatedSubnetId` on Azure — rather than in a default or per-resource
   one. This is the assertion that fails today on all three.
6. **No internet gateway** unless a `compute_instance` in the scope asked
   for `public_ip`.
7. **Sealed**: a sealed scope refuses a peering request; the default scope
   is sealed.
8. **Teardown gating**: destroying one of two resources in a scope leaves
   the network; destroying the last one removes it; a ledger with entries
   the destroy did not cover leaves it.
9. **Address derivation is deterministic and stable**: the same scope
   tuple yields the same range across runs, across processes, and — as a
   golden-file test — across releases. A change to the hash silently
   re-addresses every network that has ever been deployed, so it must
   break a test rather than a deployment.
10. **Distinct scopes get distinct ranges** within an account: a
    table-driven sweep over a realistic scope matrix (several
    environments × several regions) asserting pairwise disjointness, plus
    a fuzz target over scope tuples asserting that every derived range
    falls inside `base_cidr` and is correctly aligned.
11. **Collision is refused, not resolved**: two scopes forced onto the
    same range produce an error naming both scopes and the override,
    and no network is created. The mirror of RFC 014's
    `ErrAmbiguousProvider` test.
12. **Accounts are partitioned**: two scopes in *different* accounts
    deriving the same range apply cleanly and raise nothing. This is the
    test that pins §2.1 — a future refactor of the conflict check that
    started comparing across accounts would fail here rather than in
    somebody's console.
13. **Overrides are validated like derived ranges**: a pin outside
    `base_cidr`, a non-RFC-1918 pin, and a pin overlapping another scope
    in the same account are each rejected.

## 6. Rollout

This is the most destructive change proposed so far, and it should be read
as such before approval.

- **AWS `relational_database` moves VPC**, which forces replacement. The
  data does not survive.
- **GCP `relational_database` gains a private IP**, which replaces the
  instance.
- **Azure** resources move from a per-resource VNet to the scope's, which
  replaces the server (`delegated_subnet_id` is `ForceNew`, as RFC 015 §6
  recorded).

Every one of those is caught by the deletion protection RFC 015 made
default-on: Pulumi refuses to replace a protected resource, so an existing
deployment stops with an error rather than losing a database. Landing RFC
015 first was what made this RFC's rollout survivable, which is the
argument for the ordering having been worth it.

Sequence:

1. `EnsureNetwork` on the interface, the Engine's sequencing, and the
   stack naming — no provider builds anything yet, every implementation
   returns nil, behaviour unchanged.
2. Address derivation and the collision check, as a pure function in
   `internal/provider/network` with no cloud in it. It lands before any
   provider builds a VPC, because it is the one piece that is expensive to
   change afterwards: a range that has been deployed cannot be recomputed
   without rebuilding the network, so the algorithm must be settled and
   golden-tested before the first VPC exists.
3. AWS network + moving both AWS resource types into it.
4. GCP, including private services access — the change that makes a GCP
   database reachable for the first time.
5. Azure, collapsing RFC 015's per-resource VNets into the scope's.
6. Teardown gating on the ledger.
7. `cli.md`'s per-provider reachability table — currently three rows
   explaining what you cannot connect to — becomes one sentence.

## 7. Open Questions

1. ~~**NAT for egress.**~~ **Settled by [RFC 017 §2.7](017-container-service.md),
   and sooner than this section intended.** The proposal was to ship no
   NAT and revisit it in RFC 017, on the grounds that pulling a container
   image is the first thing that truly cannot work without egress. That
   was wrong about a resource type this RFC had already shipped:
   `compute_instance` is reached through Session Manager, and the SSM
   agent is a client that must reach the SSM endpoints before a session
   exists. With no NAT and no interface VPC endpoints, an instance boots
   into a subnet where nothing can talk to it.

   So egress is not deferred to the container service — it is the first
   step of RFC 017's rollout, ahead of the container service itself, and
   it repairs a defect rather than enabling a feature. The shape is one
   NAT gateway per scope (not per AZ, for the cost reason this section
   gave), unconditional, provisioned with the network, in a new public
   subnet tier that holds the gateway and nothing else. The `/20` layout
   in §2.4 needs no change to accommodate it.
2. ~~**`StackReference` against the DIY file backend.**~~ **Settled during
   step 3, and not the way this section expected.** Neither `StackReference`
   nor the fallback was used: resource programs find their network by
   **tag** (`ManagedBy=cloudsdd`, `CloudSDDScope=<scope>`) through the
   provider's own lookup — `ec2.LookupVpc` and `ec2.GetSubnets` on AWS.

   The reason is not that `StackReference` was proven unusable; it could
   not be proven either way, because verifying it needs the `pulumi` CLI
   against a real backend and an unverified assumption whose failure
   surfaces only on a user's first real deploy is the wrong thing to build
   on. The reason is that a `StackReference` couples a resource stack to
   the *state backend* — to another stack's name, in a local directory a
   user can move, share, or lose independently of the cloud. A tag lives
   in the account, next to the thing it describes, and answers the same
   question from whichever machine happens to be asking.

   It also keeps §2.2's interface honest: `EnsureNetwork` still returns
   only an error, so no cloud-specific identifier has to travel through
   the cloud-agnostic Engine, which is what the fallback would have
   required.

   The cost is one extra invoke per resource program and a naming
   contract — the tag keys, and the derived DB subnet group name — that
   both halves must agree on. That agreement is tested from both
   directions rather than assumed.
3. **Slot size.** §2.4.1 proposes a `/20` per scope, giving 4096 slots in
   a `/8`. A `/16` per scope would be roomier per network and leave only
   256 slots, which a hash would collide in far too readily; a `/24`
   quadruples the slots but leaves 256 addresses for a multi-AZ network
   with a NAT and endpoints. `/20` is the proposal; the number should be
   argued with rather than inherited.
4. **What the hash covers when a scope segment is empty.** An unscoped
   resource — no account, no environment — is legal today and produces a
   bare stack name. Its network's derivation therefore hashes empty
   strings, which is deterministic and correct but means "the default
   scope" is one particular range that every unscoped deployment on every
   machine shares. Proposed: acceptable, since unscoped means "I have not
   told CloudSDD where this belongs", and two such users were never going
   to connect their networks anyway.
5. **Existing deployments.** No migration path is offered: a user with a
   database in the default VPC must destroy and recreate it. Given no
   released version exists, proposed as acceptable — but it is the kind of
   decision that stops being acceptable exactly once.

## 8. Implementation notes

- `stackNameFor` already omits empty segments, so
  `stackNameFor("prod", "live", "eu-central-1", "")` yields
  `prod::live::eu-central-1`. It needs a guard so a resource can never be
  named such that it collides with a scope stack.
- The `CloudProvider` interface gains a method, so every implementation
  and every test double must be updated — including `mockProvider` in
  `internal/engine` and `pickyProvider` in `resolve_test.go`. A nil
  implementation is the correct default for a provider with nothing to
  share.
- RFC 014's candidate resolution calls `Validate` only, never
  `EnsureNetwork`: deciding which cloud can express a resource must not
  create a VPC on all three. This is worth an explicit test.
- Address derivation belongs in a new `internal/provider/network`, pure
  and cloud-free, alongside the CIDR arithmetic and the collision check —
  the same shape as `internal/schedule`, which keeps every decision about
  *time* in one testable place for exactly the same reason. Every decision
  about *addresses* should live in one place too.
- The hash must be a fixed algorithm, not `maphash` or anything seeded per
  process: `sha256` of the scope tuple with a separator that cannot occur
  in a scope segment, truncated to the slot index. `hash/fnv` would also
  do, but the property that matters is that it is written down and
  golden-tested, since changing it re-addresses every deployed network.
- The tuple must be joined unambiguously before hashing. `("a", "bc")` and
  `("ab", "c")` have to derive different ranges, so the separator is
  `stackScopeSeparator` — already chosen in RFC 005 §2.6 for being outside
  the `resourceid`, `scopename` and region charsets.
- CIDR arithmetic on `net/netip` rather than `net`: `netip.Prefix` gives
  containment and overlap checks without allocation, and the standard
  library covers everything needed here, so no dependency.
- AWS `aws.rds.Instance` needs `DbSubnetGroupName` and
  `VpcSecurityGroupIds`; GCP `sql.DatabaseInstance` needs
  `Settings.IpConfiguration.PrivateNetwork` plus a
  `servicenetworking.Connection`; Azure is already wired via
  `DelegatedSubnetId`.

---

**Do you approve this RFC?**
