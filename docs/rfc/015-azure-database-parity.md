# RFC 015: Azure Relational Database Parity

- **Status:** Approved (2026-08-01), implemented
- **Author:** Claude (Senior Staff Cloud Platform Engineer, AI-assisted)
- **Date:** 2026-08-01
- **Depends on:** [RFC 008](008-gcp-azure-providers.md),
  [RFC 011](011-provider-hardening-and-test-coverage.md),
  [RFC 012](012-environment-power-scheduling.md),
  [RFC 013](013-compute-instance.md)

## 1. Problem

`docs/cli.md` publishes a table of secure defaults. Two of its
`relational_database` rows are unqualified — they carry no provider
carve-out, unlike the rows next to them, which name `(AWS only)` and
`(GCP)` where a default is not universal:

| Resource | Default | Relax with |
|---|---|---|
| `relational_database` | Deletion protection on | `deletion_protection: false` |
| `relational_database` | Not publicly reachable | — |

Neither is true on Azure.

**Deletion protection does not exist on Azure.**
`internal/provider/azure/database.go` declares a
`relationalDatabaseProperties` with three fields; AWS and GCP declare the
same struct with a `DeletionProtection *bool` defaulting to `true`. RFC
011 §1.1E found that default shipped hardcoded *off* on both and fixed it
there, because a mistranslated destroy prompt turns into unrecoverable
data loss. Azure was not part of that fix and still has no field at all.
The user-visible consequences are two:

- an Azure database has no deletion protection, by any mechanism;
- `deletion_protection: false` — the documented way to *relax* the
  default — is rejected on Azure as an unknown property, because strict
  decoding refuses keys the struct does not declare. The escape hatch
  errors out on the provider that needs no escape hatch.

**Azure MySQL is publicly reachable.** `declareMySQLFlexibleServer` says
so itself:

```go
// Note: unlike its PostgreSQL counterpart, the MySQL Flexible Server
// resource in pulumi-azure v5 exposes no PublicNetworkAccessEnabled
// field; network isolation is achieved via DelegatedSubnetId, which
// requires a VNet this RFC does not yet model. Tracked as a follow-up
// rather than silently claimed.
```

The code declined to claim it. The documentation claimed it anyway. A
MySQL Flexible Server created with neither `DelegatedSubnetId` nor
firewall rules has its public endpoint enabled and reachable — what stops
a connection is the absence of a firewall rule, which is one portal click
or one `az` command away from being added by somebody who believes the
server was never public in the first place.

RFC 013 has since built a VNet, subnet and NSG for `compute_instance` on
Azure. The primitive the comment said did not exist now does.

### 1.1 What this RFC is not

While confirming the above, a third thing surfaced that this RFC
deliberately does **not** fix. "Private by default" is currently
implemented on all three clouds as *no configured network path*:

| Provider | Setting | What it means |
|---|---|---|
| AWS | `PubliclyAccessible: false` | Reachable from within the default VPC |
| GCP | `Ipv4Enabled: false`, no `PrivateNetwork` | No path at all: a private IP needs a VPC with private services access |
| Azure PG | `PublicNetworkAccessEnabled: false` | No path at all: needs VNet integration or a private endpoint |

Only AWS produces a database something can actually connect to. The other
two are secure in the way an unplugged server is secure. Fixing that
means giving CloudSDD a network model — which resources share a VNet,
what an `Environment` means at the network layer — and that is RFC 005 §5
territory, already deferred, and larger than this RFC. §2.3 explains why
the change proposed here nonetheless moves Azure toward it rather than
away.

## 2. Proposed Architecture

### 2.1 `deletion_protection` on Azure, through two mechanisms

Add `DeletionProtection *bool` to Azure's `relationalDatabaseProperties`,
with the same `EffectiveDeletionProtection()` helper and the same
default-true semantics as AWS and GCP. The property name, type and
default are copied deliberately: a property that means the same thing
should be spelled the same way on every cloud.

Azure has no server-side equivalent of `aws_db_instance.deletion_protection`.
The Flexible Server APIs offer no such flag. What Azure offers is a
**management lock**, and what Pulumi offers is a **protect flag**, and
they defend against different things. Enabling the property declares
both:

```go
// 1. Azure-side: blocks deletion by anyone, through any tool.
management.NewLock(ctx, id+"-lock", &management.LockArgs{
    Scope:     server.ID(),
    LockLevel: pulumi.String("CanNotDelete"),
    Notes:     pulumi.String("CloudSDD deletion_protection"),
})

// 2. Pulumi-side: blocks deletion by CloudSDD itself.
postgresql.NewFlexibleServer(ctx, id+"-server", args, pulumi.Protect(true))
```

Neither alone is sufficient, and the reason is worth recording because it
is not obvious:

- **The management lock does not stop `cloudsdd destroy`.** The lock is a
  resource in the same Pulumi stack as the server, and it depends on the
  server, so a destroy deletes the lock first and the now-unlocked server
  second. Shipping only the lock would produce a deletion protection that
  protects against every path except the one CloudSDD itself drives —
  which is the path a mistranslated prompt travels.
- **The protect flag does not stop the portal.** It lives in CloudSDD's
  state file. Anyone with `az` or a browser deletes the server without
  ever consulting it, and the next `cloudsdd` run discovers the resource
  is gone.

Together they cover both. The escape hatch is
`deletion_protection: false`, which flips both off in a single apply,
after which `cloudsdd destroy` proceeds — the same two-step flow AWS
already requires, where the flag must be changed and applied before the
instance can be deleted.

**`CanNotDelete`, never `ReadOnly`.** Azure offers both lock levels and
`ReadOnly` is the stronger one, which is exactly why it is wrong here: it
forbids modification, and RFC 012 provisions an Automation runbook whose
whole job is to stop and start this server on a schedule. A `ReadOnly`
lock would turn a power schedule into a stream of failed runbook jobs,
and the failure would surface as an unchanged bill rather than an error.

**The lock needs a permission Contributor does not have.** Creating a
management lock requires `Microsoft.Authorization/locks/write`, held by
Owner and User Access Administrator but not by Contributor. On a
Contributor-only identity, a default-on `deletion_protection` therefore
makes every Azure database deploy fail. That is the correct outcome and
matches the precedent this provider already set for
`encryption_at_host` (RFC 013): the deploy fails with an explicit error
rather than quietly provisioning less protection than the user was
promised. `docs/cli.md` gains the required role, and the error is worded
to name the permission and the one-property opt-out.

### 2.2 Network isolation for both engines, not just MySQL

MySQL Flexible Server can only be made private through VNet integration:
`DelegatedSubnetId` plus `PrivateDnsZoneId`. Both fields exist on
`mysql.FlexibleServerArgs` in the pinned SDK, as do a subnet delegation
for `Microsoft.DBforMySQL/flexibleServers` and the `privatedns` package.
So the resource graph is:

```
resource group
├── virtual network                  10.1.0.0/16
│   └── subnet                       10.1.1.0/24, delegated to the engine's service
├── private DNS zone                 <id>.{mysql,postgres}.database.azure.com
│   └── zone ↔ vnet link
└── flexible server                  DelegatedSubnetId + PrivateDnsZoneId
```

The address space is `10.1.0.0/16`, not the `10.0.0.0/16` RFC 013 gave
`compute_instance`. They occupy separate VNets today and could keep
overlapping ranges indefinitely without breaking, but the moment anything
peers them — which is the entire point of the network model in §1.1 —
identical ranges make peering impossible. Choosing distinct ranges now
costs one constant and removes a migration later.

**PostgreSQL moves to the same mechanism.** It currently uses
`PublicNetworkAccessEnabled: false`, which is a genuinely private
posture, so this is not a fix but an alignment, and it deserves the
argument:

- Leaving them different means `relational_database` on Azure has two
  different network postures and two different resource graphs, selected
  by `engine` — a property the user chose for reasons that have nothing
  to do with networking. Someone reading `type: relational_database,
  provider: azure` cannot tell what was built without also reading
  `engine`. That is the shape of divergence RFC 011 spent its length
  closing.
- The two postures are also not equally useful. A PG server with its
  public endpoint disabled and no VNet has no path at all (§1.1); a
  VNet-integrated one is reachable from its VNet. Aligning on VNet
  integration moves *toward* a connectable database rather than
  standardising on unreachable.
- `postgresql.FlexibleServerArgs` carries `DelegatedSubnetId` and
  `PrivateDnsZoneId` in the pinned SDK, so nothing needs a provider
  upgrade.

The cost is that `delegated_subnet_id` forces replacement on an existing
PG server. §6 covers it.

### 2.3 What this still does not give you

A VNet-integrated database is reachable from its own VNet, and its own
VNet contains nothing else. It is a real improvement — the endpoint is no
longer public, and there is now a network object to peer or attach to —
but it is not connectivity. Making these databases reachable from an
application requires deciding what shares a network, which is the model
§1.1 defers.

This RFC states the limitation in `cli.md` rather than letting the
secure-defaults table imply otherwise a second time. That is the whole
lesson of the two gaps being fixed here: the table's job is to describe
what the code does, and a row that overstates it is worse than a missing
row, because a reader has no reason to doubt it.

## 3. Impacted JSON Schema

No change to the Specification schema. `Resource.Properties` is
`map[string]any` at the `spec` layer and typed per provider, so this adds
a property to Azure's decoder only:

```json
{
  "id": "app-db",
  "type": "relational_database",
  "provider": "azure",
  "scope": { "region": "westeurope" },
  "properties": {
    "engine": "mysql",
    "version": "8.0.21",
    "deletion_protection": false
  }
}
```

| Property | Type | Default | Notes |
|---|---|---|---|
| `deletion_protection` | `bool` | `true` | New on Azure; already present on AWS and GCP with identical semantics |

`internal/nlp/prompt.go` currently tells the model
`deletion_protection (bool, optional, default true; AWS and GCP)`. That
qualifier is accurate today and must be dropped when this lands —
otherwise the model keeps steering the property away from the provider
that now supports it. The system prompt has been the most accurate of the
three documents throughout this defect, which is worth noting: it is the
one a machine reads back.

`docs/openapi.yaml` has schemas for `ComputeInstanceProperties`,
`S3ObjectStorageProperties` and `AWSCrossAccountRoleProperties`, and
**none for `relational_database`** — the oldest resource type in the
schema is the one type whose properties the OpenAPI document never
described. This RFC adds `RelationalDatabaseProperties` covering
`engine`, `version`, `high_availability`, `deletion_protection` and
`skip_final_snapshot`, with per-provider applicability noted the way the
other schemas already do it.

## 4. Security Considerations

| Threat | Mitigation |
|---|---|
| Mistranslated `destroy` prompt destroys an Azure database | `deletion_protection` defaults on, and `pulumi.Protect` refuses the delete at plan time, before any resource in the stack is touched |
| Out-of-band deletion (portal, `az`, another team's automation) | `CanNotDelete` management lock on the server, which no tool bypasses without removing it first |
| A schedule that silently stops working | `CanNotDelete` rather than `ReadOnly`, so RFC 012's runbook can still start and stop the server |
| Publicly reachable MySQL endpoint | VNet integration via delegated subnet; the public endpoint ceases to exist rather than being left open-but-unfirewalled |
| A firewall rule added later re-opens the server | With no public endpoint there is nothing for a firewall rule to open. This is the substantive difference from the "no rules, so nothing connects" posture being replaced |
| Documented protection that does not exist | The `cli.md` rows gain accurate provider qualifications, and §2.3's limitation is stated rather than implied |
| Deploy silently degrading to less protection than promised | A missing `Microsoft.Authorization/locks/write` fails the deploy with a named permission, following the `encryption_at_host` precedent |
| Credential exposure | Unchanged: the admin password stays a `random.RandomPassword` in encrypted state, and the credential-name denylist still rejects a user-supplied one |

Two residual risks, stated rather than mitigated:

- **The protect flag is only as durable as the state file.** A user who
  deletes their local Pulumi state loses the CloudSDD-side half of the
  protection. The Azure-side lock survives, which is a further reason to
  ship both.
- **A lock is removable by the identity that created it.** Deletion
  protection raises the cost of an accident; it does not defend against a
  determined operator with Owner, and nothing at this layer could.

## 5. Testing Plan

Table-driven, in `internal/provider/azure`, following RFC 013's pattern
of asserting on declared resources through `pulumi.WithMocks` — no
Docker, no `pulumi` CLI, no subscription.

1. **Property decoding**: `deletion_protection` absent → `true`;
   explicit `false` → `false`; explicit `true` → `true`; a non-boolean is
   rejected. The regression that matters: `deletion_protection: false`
   must stop being an unknown-property error.
2. **Declaration, protection on**: a `management.Lock` exists, scoped to
   the server, at level `CanNotDelete` and not `ReadOnly`; the server
   carries `Protect`.
3. **Declaration, protection off**: no lock resource exists at all, and
   the server is unprotected. Asserting the *absence* matters — a lock
   left behind at `false` would make the documented escape hatch a lie.
4. **Networking, both engines**: a delegated subnet exists with the
   service delegation matching the engine, the private DNS zone name ends
   in the engine's Azure suffix, a zone↔VNet link exists, and the server
   references both. Parameterised over `postgres` and `mysql` in one
   table, since the point of §2.2 is that they no longer differ.
5. **No public endpoint**: the MySQL server declares a
   `DelegatedSubnetId`. This is the assertion that would have failed
   before this RFC and is the reason it exists.
6. **Composition with RFC 012**: a scheduled *and* protected database
   declares both the Automation schedule and the lock, and the lock is
   `CanNotDelete`.
7. **Address ranges**: the database VNet does not overlap the compute
   VNet — a two-line test that pins §2.2's constant against a future
   edit.

`scripts/coverage-gate.sh` raises the `internal/provider/azure` floor to
whatever the implementation reaches; the floors ratchet upward only.

## 6. Rollout

The PG networking change forces replacement of an existing PostgreSQL
Flexible Server: `delegated_subnet_id` is a `ForceNew` field, so Pulumi
deletes and recreates, and **the data does not survive**. This is the one
genuinely dangerous part of this RFC and it needs to be said plainly
rather than buried in a note.

Mitigating factors: CloudSDD has no released version and no declared
users, so the exposure is local stacks; and the recreate is visible in
`cloudsdd deploy`'s plan output before the confirmation gate, as a
replace rather than an update.

Mitigation: **implement `deletion_protection` first** (§2.1), in its own
commit. A pre-existing PG server then already carries a lock and a
protect flag by default when the networking change lands, so the
replacement is *refused* rather than performed — the user is told, and
opts in by setting `deletion_protection: false`, exactly as they would
for any other destructive change. The ordering turns the sharpest edge in
this RFC into an instance of the protection the same RFC adds.

Sequence:

1. `deletion_protection` on Azure, property + lock + protect flag, tests,
   `cli.md` rows qualified.
2. VNet integration for MySQL: the gap that motivated the RFC.
3. VNet integration for PostgreSQL: the alignment, with the replacement
   behaviour called out in the commit message.
4. `architecture.md` "Out of scope" loses both entries and its Azure
   section is updated; `openapi.yaml` gains
   `RelationalDatabaseProperties`; `prompt.go` drops the
   "AWS and GCP" qualifier.

## 7. Open Questions

1. **Should the lock be scoped to the resource group instead of the
   server?** A resource-group lock would also protect the VNet and the
   Automation account from deletion. It would also block deleting *any*
   resource in the group, including ones a legitimate update needs to
   replace. Proposed: server-scoped, which matches the granularity of the
   property's name.
2. **Should `object_storage` get the same treatment?** An Azure storage
   account has no deletion protection either, and AWS/GCP do not offer
   the property on buckets, so today the three agree by omission. Out of
   scope here; worth its own decision if a bucket ever gains it.
3. **Should the private DNS zone be shared between resources?** One zone
   per database is simple and isolated, but Azure caps zone links per
   VNet, and a shared zone becomes the obvious design once the network
   model in §1.1 exists. Proposed: one per database now, revisit with
   that RFC.
4. **`skip_final_snapshot` has no Azure analogue** and remains
   AWS-only — correctly documented as `(AWS)` in `cli.md`, unlike the two
   rows this RFC fixes. No change proposed.

## 8. Implementation notes

- `management.LockArgs` takes `Scope` (a resource ID), `LockLevel`
  (`CanNotDelete` | `ReadOnly`), `Name` and `Notes`. Verified present in
  `pulumi-azure/sdk/v5@v5.89.3`.
- `pulumi.Protect(true)` is a `ResourceOption`; Pulumi rejects the delete
  at plan time, so a destroy fails atomically instead of part-way
  through.
- The subnet delegation is `network.SubnetDelegationArgs` with a
  `ServiceDelegation` naming `Microsoft.DBforMySQL/flexibleServers` or
  `Microsoft.DBforPostgreSQL/flexibleServers`, with action
  `Microsoft.Network/virtualNetworks/subnets/join/action`.
- Private DNS zone names must end in `.mysql.database.azure.com` or
  `.postgres.database.azure.com`; Azure validates the suffix.
- `declareRelationalDatabase` already builds the resource group and
  password before dispatching on engine. The network belongs in that
  shared prologue, since both engines now need it, and only the
  delegation name and DNS suffix vary — a two-entry lookup rather than a
  branch in each declare function.

---

**Do you approve this RFC?**
