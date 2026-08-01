# RFC 016: The Environment Network

- **Status:** Proposed
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

### 2.2 The network is its own stack, and the Engine sequences it

The network gets a Pulumi stack of its own, named by the scope it serves:

```
prod::live::eu-central-1::app-db      # resource stack, unchanged
prod::live::eu-central-1              # network stack, new
```

The name is what `stackNameFor` already produces for an empty
`Resource.ID` — the network is the scope itself rather than something in
it. Resource programs read its outputs through a Pulumi `StackReference`.

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

**AWS** — a VPC (`10.0.0.0/16`), private subnets in **at least two
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

No new fields. This RFC changes what existing ones *mean*:

| Field | Before | After |
|---|---|---|
| `scope.environment` | A label used for stack naming and ledger keying | Also the network boundary: resources sharing it share a network |
| `scope.sealed` | Consulted only by `cross_account_role` | The network's isolation posture (§2.5) |
| `scope.region` | Where the resource is created | Also which network it joins |

The absence of new fields is the point: a user who never mentions
networking gets a private network, which is what "the provider enforces
best practice without being asked" means (CLAUDE.md).

`docs/openapi.yaml` gains the reinterpretation in the `Scope` schema's
descriptions.

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
| Cross-account reachability | Out of scope and unchanged: a `DeploymentTarget` is a different account, hence a different scope, hence a different network with no route to ours |

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
2. AWS network + moving both AWS resource types into it.
3. GCP, including private services access — the change that makes a GCP
   database reachable for the first time.
4. Azure, collapsing RFC 015's per-resource VNets into the scope's.
5. Teardown gating on the ledger.
6. `cli.md`'s per-provider reachability table — currently three rows
   explaining what you cannot connect to — becomes one sentence.

## 7. Open Questions

1. **NAT for egress.** Private subnets with no NAT gateway means no
   outbound internet: no package updates, no pulling a container image
   (which RFC 017 will need). A NAT gateway costs roughly $32/month per AZ
   before traffic, which is a real cost to impose by default on a tool
   whose other headline feature is switching things off at night.
   Proposed: no NAT initially, revisited by RFC 017, which is the RFC that
   genuinely needs it.
2. **`StackReference` against the DIY file backend.** Pulumi supports it,
   with stack names unqualified by org/project. This needs verifying
   against the pinned SDK before implementation, since the whole design
   rests on it. If it proves unusable, the fallback is for `EnsureNetwork`
   to return the network's identifiers to the Engine, which passes them to
   the provider's resource programs — more plumbing, same architecture.
3. **Address ranges across scopes.** Every scope proposed here uses the
   same `10.0.0.0/16`, which is fine while nothing is peered and fatal the
   moment something is. Deriving a distinct range per scope needs an
   allocator and some persisted state. Proposed: same range for now,
   documented, and blocking on the peering feature rather than on this
   RFC.
4. **Existing deployments.** No migration path is offered: a user with a
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
- AWS `aws.rds.Instance` needs `DbSubnetGroupName` and
  `VpcSecurityGroupIds`; GCP `sql.DatabaseInstance` needs
  `Settings.IpConfiguration.PrivateNetwork` plus a
  `servicenetworking.Connection`; Azure is already wired via
  `DelegatedSubnetId`.

---

**Do you approve this RFC?**
