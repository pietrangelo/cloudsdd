# RFC 020: Scope-Owned Filesystems

- **Status:** Approved
- **Author:** Claude (Senior Staff Cloud Platform Engineer, AI-assisted)
- **Date:** 2026-08-09
- **Depends on:** [RFC 016](016-environment-network.md) §2.2 (the scope
  network and its stack), [RFC 017](017-container-service.md),
  [RFC 018](018-build-pipeline.md) §2.7 (which chose the portable subset),
  [RFC 019](019-rosette-consolidation.md) §2.4 (the shared scope helpers)

## 1. Problem

RFC 018 §2.7 decided **what** a volume is: a filesystem — EFS, Filestore,
Azure Files — and not an object store mounted to look like one. It also
stated the rule that makes volumes worth having:

> The volume belongs to the **scope**, not to the service, so two services
> in one environment can share one — which is why it is not a property of
> `container_service`.

The data model landed on that promise. `spec.Resource.Volumes` says, in
its own doc comment, that a volume "is created in the scope's network, it
outlives the resource that mounts it, and everything in the scope naming
it gets the same one."

**Nothing in the provider layer can currently deliver that sentence.** A
filesystem has exactly the property `provider.CloudProvider` already
names, in the doc comment on `EnsureNetwork`:

> It exists because a network outlives and precedes the resources in it,
> and so cannot be declared inside any one resource's program: stack
> identity is per-resource, so a program cannot create something a
> different stack also needs.

Substitute "filesystem" for "network" and the paragraph is unchanged. But
`EnsureNetwork(ctx, s NetworkScope, p spec.Policies)` receives a scope and
a policy set, and a scope cannot say which volumes the resources in it
asked for. So the only place a provider *can* declare a filesystem today
is inside one resource's program — which is the one place the architecture
says a shared thing must not be.

The consequence is not cosmetic. Two services in one environment both
naming `uploads` would each create their own filesystem, quietly, and each
would see an empty directory where the other's files were supposed to be.
That is a wrong answer delivered as a success, which is the failure mode
this project's threat model treats as worse than an error.

### 1.1 Why this could not be settled inside RFC 018

RFC 018 was a resource-type RFC: it added `build_pipeline` and it reasoned
about volumes as a *property of a workload*. The ownership question only
becomes visible when the volume has to be provisioned, because that is
when "which Pulumi stack holds this?" has to be answered — and the answer
turns out to change a core interface, which is out of scope for an RFC
about a build pipeline.

Two smaller things also went unstated there and are settled here: which
resource types can genuinely mount a filesystem (§2.4), and the fact that
`size_gb` does not mean the same thing on the three clouds (§2.7).

## 2. Proposed Architecture

### 2.1 The owner is the network stack

A scope's filesystems are declared **in the scope's network stack**,
beside the VPC, the subnets and the DB subnet group.

This is not a new idea in this codebase; it is the existing rule applied
to a second kind of shared thing. `declareScopeNetwork` already says of
the DB subnet group:

> It belongs here, with the network, rather than with the database: it
> describes the network and is shared by every database in the scope.

A filesystem is shared by every service in the scope, lives inside the
scope's network, and outlives any one of them. Placing it anywhere else
would require a resource stack to create something another resource stack
depends on, which is the coupling RFC 016 §2.2 removed.

It also gives the filesystem a **lifecycle that is already correct**: the
network stack is created by `ensureNetworks` before any resource in the
scope is applied, and removed by `ReapNetworks` only once the ledger
proves the scope is empty (RFC 016 §2.6). A shared filesystem must not
disappear when the first of two services that mount it is destroyed, and
under this ownership it cannot.

*(Rosette — Durability: the seam that changes is the one that was already
declared as the seam for shared infrastructure. Nothing new is invented,
and the next shared thing lands in the same place.)*

### 2.2 `ScopeContents` carries what the scope cannot say

`EnsureNetwork` and `DestroyNetwork` gain one parameter:

```go
// ScopeContents is what the Engine knows about a scope that the scope
// itself cannot state: what the resources in it need shared.
type ScopeContents struct {
    // Volumes are the distinct filesystems the resources in this scope
    // mount, merged by name (RFC 020 §2.3).
    Volumes []spec.Volume
}

EnsureNetwork(ctx context.Context, s NetworkScope, c ScopeContents, p spec.Policies) error
DestroyNetwork(ctx context.Context, s NetworkScope, c ScopeContents, p spec.Policies) error
```

A struct rather than a `[]spec.Volume` parameter, for one reason:
`NetworkScope` is a map key in `internal/engine` and must stay comparable,
so the contents cannot be folded into it — and the next shared thing (a
scope-wide secret store, a shared cache) then adds a field rather than a
third parameter to a five-parameter method. *(Rosette — Durability again,
and Clarity of Intent: the type's name says why it is separate from the
scope.)*

`DestroyNetwork` takes it too. It works from state rather than from the
program, but a stack cannot be selected without one, and the existing
comment on `networkProgram` is explicit that both paths must pass *the
same* program or they describe different networks.

The Engine remains the only component that sees every resource in a
Specification, which is what RFC 016 §2.2 already gives as the reason
ordering lives there.

### 2.3 Merging: a name identifies one filesystem, and a conflict is an error

Within a scope, a volume `name` identifies one filesystem. Two resources
naming `uploads` get the same one — that is the entire point.

`mount_path` is deliberately **not** part of that identity: it is where
the filesystem appears inside one container, and two services mounting the
same share at different paths is ordinary and correct.

`size_gb` **is** part of the filesystem, so two resources naming one
volume with different sizes is a Specification that cannot be honoured:
one of the two numbers would have to be discarded silently. The Engine
refuses it, with both values and both resource ids in the message.

```
engine: scope "aws::acme/prod/eu-central-1": volume "uploads" is declared
with size_gb 100 by resource "web" and 200 by resource "worker"
```

An absent size merges with a present one — "let the provider decide" is
not a competing answer, it is the absence of one.

*(Rosette — Clarity of Intent: the absence of a size is modelled as
absence, not as a zero that then loses an argument with a real number.)*

### 2.4 `container_service` only: `build_pipeline` loses its mount

`ResourceType.MountsFilesystem` currently answers true for
`container_service` **and** `build_pipeline`. That is narrowed to
`container_service` alone.

The reason is that the pipeline half is not expressible on two of the
three clouds and is expressible on the third only by reversing a decision
RFC 018 made deliberately:

| | Can the build mount a filesystem? |
|---|---|
| AWS, CodeBuild | Yes, via `fileSystemLocations` — **but only for a build placed inside the VPC**, and RFC 018 runs the build in CodeBuild's own managed network on purpose |
| GCP, Cloud Build | No NFS mount |
| Azure, ACR Tasks | No |

Shipping the field on a type where two providers must refuse it, and the
third must contradict RFC 018 to honour it, is the "accepted then not
delivered" shape that RFC 012 §1.3 rules out. A pipeline that needs a
cache is a separate feature with a separate answer (each service has a
native build cache), not a filesystem mount.

This is a narrowing of a rule that has never shipped in a release, and it
costs one `case` line, its doc comment and two test rows.

### 2.5 Names are derived, never autonamed

Every filesystem is reached from a **different stack** than the one that
created it, exactly as the DB subnet group is, so its physical name cannot
carry a Pulumi-generated suffix. Each cloud's name is derived from
`(scope, volume name)` by arithmetic both stacks can do independently —
the mechanism `dbSubnetGroupNameFor` already uses, and the reason
`provider.ShortHash` exists (RFC 019 §2.4).

The three clouds impose three different vocabularies, and they are
genuinely incompatible, so this is a per-provider derivation and not a
shared helper:

| | Identifier | Constraint |
|---|---|---|
| AWS | EFS creation token | ≤ 64 chars, free-form |
| GCP | Filestore instance name, plus a **share** name | instance `[a-z][a-z0-9-]{0,62}`; share ≤ 16 chars, alphanumeric and underscore, **no hyphen** |
| Azure | Storage account, plus a file share | account 3–24 lowercase alphanumerics, **globally unique**; share 3–63 lowercase |

Where a derived label does not fit, it is truncated and suffixed with
`provider.ShortHash` of the untruncated form — the existing convention,
which keeps two long scope names from colliding after the cut.

### 2.6 Per-provider mapping

| | AWS | GCP | Azure |
|---|---|---|---|
| Filesystem | EFS | Filestore `BASIC_HDD` | Azure Files share |
| Reachability | one mount target per private subnet, one SG allowing 2049 from the VPC CIDR | private IP peered into the scope VPC, `DIRECT_PEERING` | private endpoint in the scope VNet |
| Encryption at rest | `encrypted: true` | on by default (Google-managed) | on by default (Microsoft-managed) |
| Encryption in transit | file system policy denies `aws:SecureTransport=false`; mount uses `transitEncryption: ENABLED` | **not available on basic tiers** (§4) | SMB 3.1.1 encryption required, `minimumTlsVersion: TLS1_2` |
| Least privilege | access point pins a non-root POSIX user; IAM auth on the mount | NFS, host-level only (§4) | account key, held as a Pulumi secret (§4) |
| Backup | `BackupPolicy: ENABLED` (AWS Backup, daily) | none by default (§7.1) | share soft-delete, 7 days |
| Cost control | lifecycle policy, transition to IA after 30 days | — | — |

**AWS.** One `efs.FileSystem` per volume, one `efs.MountTarget` per
private subnet — the private tier is one subnet per availability zone
(`subnetCount`), and EFS permits exactly one mount target per zone, so the
two line up without a special case. One security group admitting NFS from
the VPC CIDR and nothing else: the mount targets sit in private subnets
with no route in from the internet, and the CIDR rule is what makes that
structural rather than incidental. An `efs.AccessPoint` per volume pins
uid/gid 1000 and a root directory of `/<volume>`, so a container cannot
write as root over the whole filesystem.

The file system policy allows `ClientMount`/`ClientWrite` only through a
mount target (`elasticfilesystem:AccessedViaMountTarget`) and denies
everything over an unencrypted connection. It grants no root access.

The consuming task role gets **its first permission ever** — RFC 017 §2.6
declared it empty on purpose — scoped to that one filesystem ARN. That is
the same rule RFC 018 applied to the build identity: write access to one
thing and nothing else.

**GCP.** One `filestore.Instance` per volume, `BASIC_HDD`, joined to the
scope VPC with `MODE_IPV4` and `DIRECT_PEERING`. `reservedIpRange` is left
unset so the service allocates from an unused range: fixing it here would
mean carving /29s out of the scope's /20 by an index that renumbers when a
volume is added, and a renumbered filesystem is a replaced filesystem.

Filestore basic tiers are **zonal**, so the instance is placed in
`<region>-a` deterministically. That is an availability caveat, stated
here rather than discovered: a zone outage takes the filesystem out even
though the Cloud Run service in front of it is regional. The regional
tiers start at a materially higher price for the same floor capacity, and
choosing one silently is not a decision to make on the user's behalf.

Cloud Run mounts it as an NFS volume. The service already has Direct VPC
egress with `ALL_TRAFFIC` through the scope network (RFC 017), so the
private IP is reachable with nothing further to configure.

**Azure.** One storage account per **scope** (not per volume — the account
is the network-attached thing and the quota lives on the share), holding
one file share per volume. `publicNetworkAccess` disabled, reached through
a private endpoint in the scope VNet with a private DNS zone, so the share
has no path from the internet at all. The Container Apps environment
declares a `managedEnvironmentsStorage` linking the share; the environment
is per-resource (RFC 017 created it inside the service's program), so this
is the one part of the consumption side that is a resource-stack
declaration rather than a mount.

### 2.7 `size_gb` means three different things

The field is one word in the Specification and three different facts in
the clouds. Rather than pretend otherwise, each provider states what it
does:

- **AWS ignores it.** EFS capacity is elastic and there is nothing to
  request. This is what `spec.Volume.SizeGB`'s doc comment already says.
- **Azure applies it as the share quota**, defaulting to 100 GiB.
- **GCP requires it, and requires it to be at least 1024.** Filestore's
  smallest instance is 1 TiB, which is roughly two hundred dollars a
  month. A user who wrote `"size_gb": 100` and got a 1 TiB bill was not
  served, and a user who omitted the field entirely and got the same bill
  was served worse. So GCP refuses both, by name, and the message states
  the floor.

Refusing rather than clamping follows RFC 012 §1.3: a request a provider
cannot express is an error, never a silent substitution. The asymmetry is
uncomfortable and it is honest — it is the point at which "cloud-agnostic"
stops being free, and the alternative is a number the user chose being
quietly replaced by one they did not.

### 2.8 Mounting is a separate step from provisioning

Provisioning puts a filesystem in the scope. Mounting is what makes it
reachable from the workload, it lives in the **resource** stack, and it
looks the filesystem up by the derived name of §2.5 — the same
lookup-not-create discipline `lookupScopeNetwork` follows, and for the
same reason: the Engine guarantees the scope's network exists before any
resource in it is applied, so a filesystem that is missing here is a
broken invariant and says so rather than creating a second one.

| | Mount |
|---|---|
| AWS | task definition `volumes[].efsVolumeConfiguration` + container `mountPoints` |
| GCP | `cloudrunv2` template `volumes[].nfs` + `volumeMounts` |
| Azure | environment storage + template `volumes[].storageName` + `volumeMounts` |

### 2.9 Destroying a filesystem destroys the data

`ReapNetworks` removes a scope's network once the ledger says the scope is
empty, and under §2.1 that now takes the filesystems with it. This is the
correct lifecycle and it is also the one irreversible thing in this RFC,
so the posture is stated rather than left to the provider defaults: AWS
enables EFS backups, Azure enables share soft-delete, and GCP has neither
by default (§7.1). A destroy therefore leaves a recovery point on two of
three clouds.

## 3. Impacted JSON Schema

The `volumes` array landed with RFC 018 §2.7 and its shape is unchanged:

```json
"volumes": [
  { "name": "uploads", "mount_path": "/var/lib/uploads", "size_gb": 100 }
]
```

One rule changes: `volumes` is accepted on `container_service` only, where
it was previously accepted on `build_pipeline` too (§2.4). No field is
added, renamed or removed.

## 4. Security Considerations

- **Nothing is reachable from the internet.** EFS mount targets live in
  private subnets behind a security group whose only ingress is NFS from
  the VPC CIDR; Filestore holds a private peered address; the Azure
  storage account disables public network access entirely and is reached
  through a private endpoint. None of the three is a default — each is an
  explicit argument, because each service's default is more open.
- **Encryption at rest is on everywhere**, explicitly on AWS and by
  service default on GCP and Azure.
- **Encryption in transit is not uniform, and the gap is real.** AWS
  enforces it twice (the file system policy denies unencrypted access, and
  the mount requests TLS). Azure requires SMB 3.1.1 encryption. Filestore
  basic tiers offer no in-transit encryption — the traffic stays inside the
  scope VPC, which is a mitigation and not equivalence. Stated here so it
  is a known limit rather than an assumed guarantee.
- **The Azure storage account key is a credential in the flow.** It is
  read by an invoke in the resource stack and handed to the Container Apps
  environment. It is marked `pulumi.Secret` so it is encrypted in state and
  never printed in a plan, which is the same handling RFC 007 gave the
  generated database password.
- **Least privilege on the mount.** The EFS access point pins a non-root
  POSIX user and grants no root access, and the task role's grant names one
  filesystem ARN. NFS on Filestore has no equivalent control: any host on
  the peered network that can reach the address can mount it. That is a
  property of the protocol, and it is why the network posture above is the
  primary control on GCP rather than a second layer.
- **A shared filesystem is a shared blast radius.** Two services in one
  scope naming one volume can read and write each other's files — which is
  what was asked for, and worth saying plainly, because the isolation
  boundary is now the scope and not the service.

## 5. Testing Plan

- `internal/spec`: the `MountsFilesystem` narrowing, table-driven, with
  `build_pipeline` moving from the accepting group to the refusing one.
- `internal/engine`: volume collection and merge — distinct scopes, the
  same name merging across two resources, an absent size merging with a
  present one, and the conflicting-size refusal naming both resources.
- Each provider, against the Pulumi mock monitor already in place: the
  filesystem's secure defaults (encryption, private reachability, backup
  posture), the mount-target-per-private-subnet arithmetic on AWS, the
  derived names, and the `size_gb` rule of §2.7 including GCP's two
  refusals.
- Each provider's mount: that the declared workload actually references the
  filesystem it was given, and that a missing filesystem is refused rather
  than created.
- No assertion in an existing test changes. The interface change of §2.2
  is a signature edit at every call site, and a test that needs a changed
  expectation means this RFC moved behaviour it did not intend to.

## 6. Rollout

1. `spec`: narrow `MountsFilesystem` to `container_service` (§2.4).
2. `provider`: `ScopeContents`, and the two interface signatures (§2.2).
3. `engine`: collect and merge the volumes of each scope, refuse a
   conflict, pass them to both methods (§2.3).
4. The three providers' `EnsureNetwork`/`DestroyNetwork` take the new
   parameter and ignore it — the tree stays green and every existing test
   still passes.
5. AWS: provision, then mount.
6. GCP: provision, then mount.
7. Azure: provision, then mount.
8. `architecture.md`, `cli.md`, and the translator system prompt.

Steps 1–4 are one refactor with no behaviour change; a filesystem appears
in step 5. Splitting them that way is what keeps the interface change
reviewable on its own, rather than arriving inside the first provider that
needed it — which is the mistake RFC 019 §1 was written to stop repeating.

## 7. Open Questions

### 7.1 Filestore has no backup by default, and this RFC leaves it that way

AWS and Azure both have a one-argument answer (`BackupPolicy`, share
soft-delete). Filestore's is a separate `filestore.Backup` resource plus a
schedule to drive it, which is a third scheduled component in a codebase
that already runs two (RFC 012's power schedule, RFC 018's purge task).
Deferred rather than dismissed: §2.9 states the asymmetry so a user is not
surprised by it, and the answer belongs with whichever RFC unifies the
scheduled-maintenance story.

### 7.2 A volume declared and never mounted

Under §2.1 a volume reaches the network stack from the resources that
declare it, so an unmounted volume cannot exist — remove the last resource
naming it and it is reaped with the scope. The intermediate state (the
volume is dropped from a resource, the scope stays alive) leaves an
orphaned filesystem that nothing mounts and that still bills. Detecting
that is the same shape as RFC 016 §2.6's occupancy check and is not
attempted here.

## 8. Implementation notes

*(Filled in as the rollout proceeds.)*

**GCP instance name (§2.5).** `filestoreInstanceNameFor` always appends
`provider.ShortHash` of the unfolded `ScopeName + "::" + volume`, rather
than only when the label overflows. A volume name admits uppercase and
underscores and GCP does not, so folding is lossy (`Cache`/`cache`,
`a_b`/`a-b`, `a-`/`a` after trimming); hashing every time makes the name
injective with no truncation branch, and `cloudsdd-fs-` + 32 + `-` + 8
never exceeds 63. The scope is readable in the instance description
instead of the name. The golden value is pinned in the test.

**GCP share name.** One constant, `filestoreShareName`, not a
per-volume derivation: each instance carries exactly one share, so the
instance name already identifies the volume, and deriving a ≤16-character
letter-first share name from a 32-character volume would need a second
truncate-and-hash rule for no gain in identity.

**GCP zone.** Set through `location` (`<region>-a`), not the deprecated
`zone` input.

**GCP wiring.** `declareScopeNetwork` takes `provider.ScopeContents` as its
fourth argument, as on AWS, and ends in `declareScopeFilesystems`; the two
`network_test.go` calls take an empty `ScopeContents{}` — the priced §5
signature exception, no assertion changed. The instance joins the VPC by
`vpc.Name` rather than a literal, so Pulumi orders it after the network.
The zone reuses `instanceZone(region, "")` so Compute Engine and Filestore
cannot disagree on what "the region's first zone" means. The share is named
`data`.

**GCP mount contract (tests).** The mock's `getInstance` answers with an
address derived from the instance name, so the two-volume test can tell
which server each mount path reaches; one volume is `Shared_Cache`, which
the Specification admits and a Cloud Run volume name (a DNS label) does
not, so the Specification's name cannot be passed through verbatim. The
lookup must name the zone (`instanceZone(region, "")`): basic tiers are
zonal, and a lookup falling back to the provider default finds nothing.
A mounting template must set `EXECUTION_ENVIRONMENT_GEN2`, since Cloud Run
mounts NFS only there; a template that mounts nothing must leave it unset,
because changing it rolls a new revision of every existing service.
Mounting grants the service identity nothing — basic Filestore has no
data-plane IAM (§4). `ErrFilesystemMissing` covers both an absent instance
and one with no private address, as the AWS sentinel covers both of its
halves, and the refusal must precede the service account and the service.
`TestDeclareContainerServiceWithoutVolumesMountsNothing` passes before the
implementation by design: it pins unchanged behaviour, not new behaviour.

**GCP mount (implementation).** The lookup (`lookupMountedFilesystems`,
`privateAddress`) lives in `filesystem.go` beside `declareScopeFilesystems`,
as on AWS, so both readers of `filestoreInstanceNameFor` sit in one file; it
takes the scope from `provider.ResourceScope`, the same derivation the Engine
uses, so a scope that gains a field reaches both halves together. It also
touches `errors.go` (`ErrFilesystemMissing`), which the item did not name.
**The Cloud Run volume is named after the instance**, not after a second
derivation from the volume name: the instance name is already a DNS label
and already injective over volume names. `mountFilesystems` returns early
when nothing is mounted, so such a template carries no `volumes` key and no
`executionEnvironment`, which avoids a new revision on every existing service.
`readOnly: false` is stated explicitly, as on AWS. An instance reporting no
network, or a network with no address, is refused. The first address of the
first network is taken without further checks, because the network stack
declares exactly one network in `MODE_IPV4`. **One test-side change:**
`container_test.go` gained `stringOrEmpty`, because two mutations (dropping
the lookup's `location`, dropping `executionEnvironment`) were killed by a
`StringValue`-on-null panic rather than by their diagnostic. No assertion
changed; they now fail with their own message.

**Azure filesystem contract (tests).** Written against the classic
`pulumi-azure` v5 inputs, so the plan's names map as
`publicNetworkAccess` → `publicNetworkAccessEnabled: false` and
`minimumTlsVersion` → `minTlsVersion: TLS1_2`. "SMB 3.1.1 encryption
required" means `shareProperties.smb` allows only `SMB3.1.1` and
`AES-256-GCM`: leaving an older dialect enabled next to it would let a client
negotiate an unencrypted channel. Share soft-delete is
`shareProperties.retentionPolicy.days: 7`. **The account name** is
`storageAccountNameFor(subscriptionID, scope)`, pinned as
`cloudsdd` + the first 16 hex digits of sha256(subscription, NUL,
`ScopeName`), which is exactly 24 characters. The subscription is included for
the reason RFC 018 §2.8.1 gave for ACR: the account namespace is global, so the
network stack must ask `getClientConfig` exactly as the pipeline does. The mock
already answers that call. A digest of 64 bits rather than `ShortHash`'s 32
suits a namespace shared with every Azure customer. **The share name** is
`fileShareNameFor(volume)`, derived from the volume alone because the account
already identifies the scope, and it always carries `ShortHash(volume)` for the
injectivity reason given for the GCP instance above. `uploads` → `uploads-9ba88c41`.
The adversarial table includes `_`, `___` and `---`, which fold to nothing, and
`a__b`, which folds to a double hyphen that Azure rejects. **The private
endpoint** sits in the general subnet, because every other subnet in the scope
is delegated and refuses one. It is a non-manual connection to the `file`
subresource and registers in a `privatelink.file.core.windows.net` zone linked
to the VNet. With no volumes, no account, share, endpoint or file zone is
declared, so `TestDeclareScopeNetwork`'s two-zone count still holds. Its call
sites in `network_test.go` gain `provider.ScopeContents{}` in the
implementation item, the priced §5 signature exception, as on GCP.
`filesystem_test.go` carries nil-safe accessors (`stringOrEmpty` and friends)
from the start, so a missing input fails with its own diagnostic rather than a
panic, which is the lesson of the GCP mount notes. **Open, not in this item:** an
Azure share accepts at most 5120 GiB unless `largeFileShareEnabled` is set,
while `size_gb` admits up to 65536. Azure's `Validate` does not yet refuse the
gap, and the plan has no item for it.

**Azure filesystem (implementation).** `filesystem.go` holds the derivations
and `declareScopeFilesystems`, which `declareScopeNetwork` calls last, handing
it the general subnet. It returns before `getClientConfig` when there are no
volumes, so a scope that mounts nothing makes no new invoke and no new
resource. The production zone constant is `filePrivateDNSZone`, because the
test file already pins the literal as `fileZoneName` and the two must not be
one symbol. A test that recomputed the value from the code it checks would
assert nothing. The account states only what the tests pin, plus the two
required fields (`Standard`, `LRS`). Blob-side switches such as
`allowNestedItemsToBePublic` are left at their defaults: with public network
access disabled they open no path, and setting them without a test would be
unguarded configuration. `sharedAccessKeyEnabled` stays on, and so do SMB
`authenticationTypes`, because the Container Apps storage link of the next
item authenticates with the account key. `DestroyNetwork` passes the contents
too, so its program describes the same graph that `EnsureNetwork` built. The
adversarial pass broke 25 guarantees in `filesystem.go` and at the
`network.go` call site, and all 25 were killed by their own diagnostics.
`EnsureNetwork`/`DestroyNetwork` forwarding `contents` is not unit-tested,
because those need a live stack, as on GCP. `gosec` and `govulncheck` are not
installed on this machine. `staticcheck` reports only two unused constants
that were already in `compute.go`.
