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

**Azure mount contract (tests).** The key-listing invoke is
`azure:storage/getAccount:getAccount`: classic `pulumi-azure` v5 has no
separate list-keys call, and `getAccount` returns `primaryAccessKey`. The same
lookup also proves the account exists. The mock derives each key from the account name
(`testStorageAccountKey`), so an assertion can tell which account's key reached
which link. The account is looked up **exactly once** per service, by
`storageAccountNameFor(subscription, scope)` in `scopeResourceGroupName`,
because it is one credential for the scope, read once. Each share is then
confirmed with `azure:storage/getShare:getShare` under `fileShareNameFor`, and
a missing account or share is `ErrFilesystemMissing`, refused before the
resource group, the identity or the environment is registered. The mount is
proved by a join (mount path → template volume → `storageName` →
`EnvironmentStorage` → `shareName`) over two volumes, one of them
`Shared_Cache`, as on GCP. Each link is `ReadWrite`, is tied to this service's
environment ID and states `storageType: AzureFile`. Left unset, that last field
defaults to `EmptyDir`, which accepts writes and loses them. **Secrecy is
pinned twice.** The link's `accessKey` must be a secret. That half holds by
construction, because `NewEnvironmentStorage` wraps the field with
`pulumi.ToSecret` itself. `TestDeclareContainerServiceKeepsTheStorageKeySecret`
therefore also walks every input of every recorded resource and fails if the
key appears anywhere outside a secret, such as an app secret, an environment
variable or a tag. That walk is what a hostile edit cannot get past. Mounting
declares no role assignment, because the link authenticates with the key and
RFC 017 §2.6's identity stays empty.
`TestDeclareContainerServiceWithoutVolumesMountsNothing` passes before the
implementation by design, and also forbids `getClientConfig` for an
image-named service that mounts nothing. `mockMonitor` gained a `files
fileScope` field, whose zero value is a scope holding everything asked for.
`container_test.go` gained `arrayOrEmpty` beside the existing nil-safe
accessors.

**Azure mount (implementation).** The plan item was amended to name
`filesystem.go` and `errors.go` as well as `container.go`.
`lookupMountedShares` sits beside `declareScopeFilesystems`, the side that
creates what it finds, as it does on GCP. It returns a `scopeShares` value: the
account, its key, and one `mountedShare` per volume. The zero value mounts
nothing and asks for no subscription. That value renders its own template
volumes and container mounts, as nil slices when empty, so a service with no
volumes declares exactly what RFC 017 gave it, not `volumes: []`. The
storage link, the template volume and the mount all take the share's name.
It is at most 41 characters (a 32-character volume name, a hyphen and an
8-character hash). Whether Container Apps caps an environment storage name
lower than that is **unverified** against a live subscription. The app
`DependsOn` every link, because a template volume names its link by name and
not by an output. The adversarial pass ran 29 mutants and killed 26. Three
survived. `key-plain` is equivalent: `NewEnvironmentStorage` already wraps
`accessKey` in `ToSecret`, as the test notes record. `no-links` (the
`DependsOn` removed) survived because no test pins ordering, and
`account-no-name` survived because the mock's own error already echoes the
account name. The last two have a follow-up test item in the plan.

**Azure mount (follow-up pins).** The recorder now keeps each resource's
dependency URNs, which Pulumi already passes the mock in `RegisterRPC`.
`assertAppWaitsForEveryLink` then requires the app's dependencies to hold
the URN of every storage link. The mount test declares two volumes, so a
`DependsOn` on the first link alone fails as well as none at all. The
mock's `getAccount` and `getShare` errors no longer echo the name they were
asked for. That leaves the provider's own message as the only place the
refusal test can find the account name. The share row now expects the
derived share name, not the volume name. The volume name was the looser
check: a message that dropped the share still carried `uploads`. All four
mutants (`no-links`, `first-link-only`, `account-no-name`, `share-no-name`)
fail with their own diagnostics. `gosec` and `govulncheck` are installed in
`~/go/bin`. They were reported missing earlier only because that directory
is not on `PATH`. `gosec` reports three G703 findings that predate this RFC,
none in a file this RFC touched. They now have their own plan item.
`govulncheck` was OOM-killed twice on this 7 GB machine and has not run.

**`gosec` G703 (Phase 5).** The GCP and Azure state dirs and the config dir
now carry the same `#nosec G703` justification that
`aws/provider.go` already had, rather than a fix. The path comes from an
env var the operator sets, or from their home directory. It never comes
from a Specification or a prompt. A confinement rule such as "must sit
under `$HOME`" would guard against the one party who already owns the
filesystem. Removing any one suppression on its own brings its finding
back. `gosec` finds nothing only if `go` is on its `PATH`: without
`/usr/local/go/bin` it loads zero files and still reports 0 issues, which
looks like a pass. With `GOMAXPROCS=2 GOGC=50 GOMEMLIMIT=4GiB`,
`govulncheck` finished on this machine for the first time. It reports six
reachable vulnerabilities in `grpc`, `x/crypto` and `go-git/v6`. None comes
from this RFC's changes, and they now have their own plan item.

**Phase 5, dependency bump.** The plan's grpc floor (≥ v1.83.1) was one
patch short: GO-2026-6443 is fixed only in v1.83.2, so grpc went to v1.83.2.
`x/crypto` went to v0.56.0 and `go-git/v6` to v6.0.0-alpha.5. `go mod tidy`
also carried minor bumps of the `x/*` family, otel and genproto. There are
zero reachable findings now. One module-level entry remains, GO-2026-5932
(`x/crypto/openpgp`, unmaintained, no fix). No package here calls it, so it
is accepted rather than worked around. On this 7 GB machine, compiling
`pulumi-gcp/.../compute` gets OOM-killed under default parallelism whenever
its cache is cold. `go test -p 1 ./...` is the reliable way to run the suite
after a dependency change; this is not a code failure.

**Phase 5, floor ratchet.** The floors now sit at the integer part of each
package's actual coverage: engine 95 → 96 (96.1%), spec 91 → 93 (93.4%),
azure 71 → 76 (76.2%), gcp 72 → 77 (77.7%). `internal/provider` is already
at 100 and cannot go higher. The provider packages went *up* even though
RFC 020 adds Pulumi-bound surface, because the filesystem and mount logic
around each declaration is asserted under the mock monitor. So the
exception in the script's header was not needed, and no dated entry was
added. Moving gcp to 78 fails with `77.7% < 78% floor`, so the new floors
bind. The script's "sit at 70-72%" comment was changed to 70-77%.

Two faults in the gate turned up along the way. Neither was fixed in this
item. **(1)** When `go` is not on `PATH`, `go list` fails inside
`$(… | grep … || true)`. The `|| true` swallows that failure, and all
eight invariant checks print `ok` while the gate exits 0. This was
reproduced with `PATH=/usr/bin:/bin`. In other words, the checks pass
when they cannot run at all. **(2)** Under a comma-decimal locale, awk
prints the percentages as `94,4`, and `a + 0` then reads them as their
integer part. That can only make the gate stricter, never looser, so it
is cosmetic. Run it with `LC_ALL=C`. Fault (1) was added to the plan as its own item.

**Phase 5, gate fault (1) fixed.** Each invariant check now runs `go list`
by itself and fails if it errors. Only after that does it filter the
output. `|| true` now sits on the `grep` alone, where an empty match is the
passing case. Tested with a fake `go` on `PATH`: when it reports
`cloudsdd/internal/engine`, all eight checks still fail. When it reports
nothing, all eight pass. When `go` is missing, all eight fail and name
`go list`. Two mutations were tried. Dropping `|| true` from the grep kills
a clean run. Ignoring the `go list` status brings back the silent `ok`
lines. Fault (2) is fixed by `export LC_ALL=C`. That could not be
reproduced here: this container has `mawk` and no comma-decimal locale.

The check turned up two failures that were already on `main` before this
item. **(a)** CI's coverage gate has been red since the Phase 5 ratchet.
CI measures aws 67.7%, azure 73.2% and gcp 74.4%, below the 70/76/77
floors. The same numbers come out in this container, so the actuals the
ratchet recorded came from some other measurement. **(b)** CI's
`govulncheck` reports four standard-library vulnerabilities, fixed in Go
1.26.6. Each has its own plan item. This container cannot check (b):
the proxy blocks `vuln.go.dev`. It also runs as root, so the
permission-denied tests in `config` and `state` skip themselves and those
two packages read below their floors here, but not in CI.

**Phase 5, failure (a) fixed: floors reset to CI's figures.** engine
96 → 95, aws 70 → 67, azure 76 → 73, gcp 77 → 74, recorded as a dated
entry in the gate's own note. The ratchet's 96.1% for engine is
`internal/provider/pipeline`'s figure. No run has ever measured engine at
96.1% or azure/gcp at 76/77, so this is a recording error being undone,
not coverage lost. azure and gcp still end two points above their
pre-ratchet floors (71, 72). aws alone uses the gate's narrow exception,
because Phase 2's EFS declarations are Pulumi-bound surface. The ratchet
never touched it. **Verification without root:** the suite was re-run as
`nobody`, from a copy of the tree. Under root, the permission-denied tests
skip, so `cmd/cloudsdd`, `config` and `state` read 92.8/89.1/86.0 here. As
`nobody` they read 93.2/93.5/91.7, the same as CI. Every figure matched
run 35771425790 to the decimal, and the gate exited 0. **Rule going
forward:** read a floor from CI's log, or from a non-root run, and never
from a root container alone.
