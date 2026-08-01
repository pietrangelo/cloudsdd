# RFC 013: Compute Instance

- **Status:** Approved (2026-08-01), implemented
- **Author:** Claude (Senior Staff Cloud Platform Engineer, AI-assisted)
- **Date:** 2026-08-01
- **Depends on:** [RFC 001](001-core-architecture-and-json-schema.md),
  [RFC 002](002-aws-provider.md), [RFC 005](005-account-environment-region-scoping.md),
  [RFC 008](008-gcp-azure-providers.md),
  [RFC 011](011-provider-hardening-and-test-coverage.md),
  [RFC 012](012-environment-power-scheduling.md)

## 1. Problem

`compute_instance` has been in the schema since RFC 001 and is implemented by no
provider. The gap is not passive: `internal/nlp/prompt.go` tells the model, in
the shared system prompt, that it is one of the valid values for
`resources[].type`. A user who asks for a virtual machine therefore gets a
translation that validates against the schema and then dies at the provider:

```
$ cloudsdd deploy "a small ubuntu VM in eu-central-1"
...
Error: engine: resource "app-vm": aws: unsupported resource type: "compute_instance"
```

This is the same failure class RFC 011 was written to close, one layer up. There
the defect was code that silently produced infrastructure other than what was
described; here it is a **published contract that advertises a capability
nothing delivers**. The user finds out after the translation, after the ledger
read, and after they have been shown a plan that cannot run.

Two other values in that same prompt line have the same shape —
`container_service` and `provider: "agnostic"` — and are out of scope here
(§7.3).

Compute is also where RFC 012 pays off. A `db.t3.micro` left running overnight
costs a few euro a month; a fleet of dev VMs is where the invoice actually
lives, and start/stop of virtual machines is the canonical scheduling case. RFC
012 already anticipated this: `ResourceType.SupportsSchedule` returns true for
`compute_instance`, and the compiler's rule set needs no change to serve it.

## 2. Proposed Architecture

### 2.1 Properties

Cloud-agnostic, in the spirit of the existing types: the user says what they
need, and each provider picks the SKU and image that expresses it. This mirrors
what `relational_database` already does — nobody writes `db.t3.micro` in a
Specification today.

```json
{
  "id": "build-agent",
  "type": "compute_instance",
  "provider": "aws",
  "scope": { "environment": "dev", "region": "eu-central-1", "zones": ["eu-central-1a"] },
  "properties": {
    "size": "small",
    "os": "ubuntu-22.04",
    "disk_size_gb": 30
  }
}
```

```go
type computeInstanceProperties struct {
    Size string `json:"size" validate:"required,oneof=small medium large"`
    OS   string `json:"os" validate:"required,oneof=ubuntu-22.04 ubuntu-24.04 debian-12"`

    // DiskSizeGB is the root volume size. Defaults to 20, which every
    // supported image boots comfortably in.
    DiskSizeGB int `json:"disk_size_gb,omitempty" validate:"omitempty,min=8,max=1024"`

    // PublicIP is *bool, not bool, so its absence activates the secure
    // default rather than the zero value (RFC 011 §2.5).
    PublicIP *bool `json:"public_ip,omitempty"`
}
```

There is deliberately **no SSH key property**. See §2.4.

#### Size mapping

Approximate parity at two vCPUs per tier, chosen from each provider's
general-purpose burstable families:

| `size` | AWS | GCP | Azure |
|---|---|---|---|
| `small` | `t3.small` | `e2-small` | `Standard_B1ms` |
| `medium` | `t3.medium` | `e2-medium` | `Standard_B2s` |
| `large` | `t3.large` | `e2-standard-2` | `Standard_B2ms` |

Parity is approximate by construction — the families do not line up exactly on
memory — and that is the honest cost of a cloud-agnostic size. Whether an
explicit per-provider SKU escape hatch is also needed is §7.1.

#### Image mapping

| `os` | AWS | GCP | Azure (publisher/offer/sku) |
|---|---|---|---|
| `ubuntu-22.04` | AMI lookup, owner `099720109477`, name `ubuntu/images/hvm-ssd/ubuntu-jammy-22.04-amd64-server-*` | `ubuntu-os-cloud/ubuntu-2204-lts` | `Canonical/0001-com-ubuntu-server-jammy/22_04-lts-gen2` |
| `ubuntu-24.04` | owner `099720109477`, `ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*` | `ubuntu-os-cloud/ubuntu-2404-lts-amd64` | `Canonical/ubuntu-24_04-lts/server-gen1` |
| `debian-12` | owner `136693071363`, `debian-12-amd64-*` | `debian-cloud/debian-12` | `Debian/debian-12/12-gen2` |

The enum holds only images that exist on all three clouds. Amazon Linux is
excluded on purpose: adding it would make `os` a value whose validity depends on
`provider`, which is exactly the kind of hidden coupling this schema avoids
(§7.2).

**AWS needs a Pulumi invoke here.** AMI IDs are region-specific, so
`ec2.LookupAmi` runs inside the program. RFC 012 §4.1 went out of its way to stay
invoke-free; that was a choice available there because the ARN already carried
the answer, and it is not available here. The invoke is confined to the AWS image
resolution, filtered by owner ID (never by name alone, which would let anyone
publishing a similarly-named public AMI win the lookup), and the resolved ID is
surfaced in the resource outputs so the plan records what was actually chosen.

### 2.2 Secure defaults

Per CLAUDE.md's CRITICAL REQUIREMENT, applied without being asked:

| Control | AWS | GCP | Azure |
|---|---|---|---|
| No public address | no `associatePublicIpAddress` | no `accessConfig` block | no public IP on the NIC |
| Encrypted root disk | `rootBlockDevice.encrypted = true` | encrypted at rest by default | `encryptionAtHostEnabled = true` |
| Metadata service | **IMDSv2 required**: `httpTokens = "required"`, `httpPutResponseHopLimit = 1` | `block-project-ssh-keys = TRUE` | n/a |
| Boot integrity | — (instance-family dependent) | Shielded VM: secure boot, vTPM, integrity monitoring | Trusted Launch: `secureBootEnabled`, `vtpmEnabled` |
| Inbound network | security group with **no ingress rules at all** | firewall: no ingress | NSG: no inbound allow rules |
| Interactive access | SSM Session Manager via instance profile | OS Login (`enable-oslogin = TRUE`) | AAD login extension |
| Serial console | — | `serial-port-enable = FALSE` | disabled |

`public_ip: true` is the single documented escape hatch, and it does **not**
open any inbound port: a public address with a default-deny security group is
still unreachable. Opening ports is a separate resource type this RFC does not
introduce.

**IMDSv2 is the load-bearing one.** The classic EC2 compromise is an SSRF in an
application that reaches the instance metadata service and walks away with the
role's credentials. `httpTokens = "required"` breaks that chain, and a hop limit
of 1 stops a container on the host from reaching it. Shipping a VM without it
would put a credential-theft primitive in every environment CloudSDD creates.

### 2.3 Scope: the first consumer of `zones`

RFC 005 §2.4.3 introduced `Scope.Zones` and noted that no resource type consumed
it. `compute_instance` is the first that can: a VM lives in exactly one
availability zone.

- Zero zones: the provider lets the cloud choose.
- Exactly one zone: pinned there.
- More than one: **error** (`ErrMultipleZonesNotSupported`). A single instance
  cannot span zones, and quietly using the first entry would hand the user
  infrastructure that does not match what they wrote. Spreading across zones
  needs an instance group, which is a distinct resource type.

`Scope.Region` stays required, as for every other regional type.

### 2.4 Access without SSH keys

No `ssh_public_key` property, and no key pair provisioned for the user to hold.
Each cloud has a first-party path that authenticates against its own IAM and
leaves an audit trail — SSM Session Manager, OS Login, the AAD login extension —
and all three work without an inbound port or a public address. A key in the
Specification would also be a long-lived credential in a document that RFC 001
§3 treats as hostile input.

**One provider forces a compromise.** Azure's `LinuxVirtualMachine` rejects a
configuration with neither an admin SSH key nor password authentication. The
proposal is to generate an ed25519 key pair in the program with
`tls.NewPrivateKey`, hand the public half to the VM, and leave the private half
in the passphrase-encrypted Pulumi state — the same shape as the RDS master
password today (RFC 007). It is never rendered to output, never written to the
ledger, and never leaves the state backend. `DisablePasswordAuthentication`
stays true.

This adds `pulumi/pulumi-tls` (Apache-2.0, compatible with the AGPLv3 audit in
`docs/dependency-licenses.md`, which is updated as part of the rollout).

### 2.5 Composition with RFC 012

Scheduling needs no change to `internal/schedule`; each provider gains a compute
path alongside its database one.

| Provider | Mechanism | Exception windows |
|---|---|---|
| AWS | EventBridge Scheduler universal targets `arn:aws:scheduler:::aws-sdk:ec2:{start,stop}Instances`, payload `{"InstanceIds": ["i-…"]}` | Supported |
| GCP | **Native** `compute.ResourcePolicy` with `instanceSchedulePolicy` | Not supported |
| Azure | Automation runbook, ARM `start` and **`deallocate`** | Supported |

Two findings worth stating precisely.

**GCP gets simpler, not more capable.** A VM can carry an instance schedule
resource policy, which takes a start cron, a stop cron and a timezone directly —
no Cloud Scheduler job, no service account, no custom role, none of the
machinery RFC 012 §4.2 needed for Cloud SQL. The policy also has `startTime` and
`expirationTime`, which looks like it closes RFC 012's exception-window gap. It
does not: an instance accepts **one** such policy, carrying **one** start/stop
pair and **one** validity interval, whereas an exception window compiles into a
segmented set of bounded rules. So the refusal stands on GCP, for a different
structural reason than Cloud SQL's, and the error message should say which.

**Azure must deallocate, not stop.** A `stop` on an Azure VM leaves it in
`Stopped` state and **still billing for compute**; only `deallocate` releases the
hardware and stops the charge. A scheduling feature whose whole purpose is cost
reduction, wired to the wrong verb, would run correctly and save nothing — and
the failure would be invisible until the invoice. The runbook's `stop` action
maps to `deallocate` on this resource type.

## 3. Impacted JSON Schema

| Change | Field | Compatibility |
|---|---|---|
| Newly usable | `resources[].type: "compute_instance"` | Additive: previously a hard error on every provider |
| Added | `properties.size`, `properties.os` | Required for the new type only |
| Added | `properties.disk_size_gb`, `properties.public_ip` | Optional, secure defaults |
| Behavior | `scope.zones` | Now consumed; more than one entry is an error on this type |

No existing Specification changes meaning.

## 4. Security Considerations

- **IMDSv2 and the hop limit** (§2.2), the control that turns an application
  SSRF from a credential compromise into a failed request.
- **No inbound rules and no public address by default.** The user gets a VM they
  can reach through their cloud's IAM-authenticated session service, not one
  exposed to the internet with a password.
- **No key material in the Specification.** The Azure-mandated key pair is
  generated in-program and stays in encrypted state (§2.4); nothing is echoed to
  stdout or to the ledger. The credential-name denylist in
  `internal/provider/decode` already rejects a property called `private_key`, so
  a user cannot supply one either.
- **Image supply chain.** AMI lookups filter on owner ID as well as name.
  Filtering on a name pattern alone would let any AWS account publishing a public
  AMI with a matching name be selected, which is a straightforward path to
  running someone else's image.
- **Azure `deallocate` vs `stop`** (§2.5): a cost control that silently does not
  control cost is worse than none, because it is trusted.
- **Blast radius of a compute default.** Every other type in this schema is a
  managed service. A VM runs arbitrary code with an attached identity, so the
  instance profile / service account created here grants exactly what the session
  service needs and nothing more — on AWS, `AmazonSSMManagedInstanceCore` and no
  additional policy.

## 5. Testing Plan

Table-driven and native, >90% where not bounded by the Pulumi Automation API.

| Package | Focus |
|---|---|
| `internal/provider/aws` | `pulumi.WithMocks`: `httpTokens == "required"`, hop limit 1, `rootBlockDevice.encrypted`, no public IP unless asked, security group with zero ingress rules, instance profile scoped to SSM. AMI filter includes an owner. |
| `internal/provider/gcp` | Shielded VM flags all three true, no `accessConfig`, `enable-oslogin` and `serial-port-enable` metadata, image family per `os`, zone honored. |
| `internal/provider/azure` | `secureBootEnabled`, `vtpmEnabled`, `encryptionAtHostEnabled`, no public IP, `disablePasswordAuthentication` true, generated key used and private half never an output. |
| all three | Size and OS mapping tables, including that an unmapped value is an error rather than a fallback. |
| scheduling | AWS payload targets the instance ID; GCP emits a resource policy and rejects exception windows with the compute-specific message; Azure's stop action resolves to `deallocate`. |
| `internal/engine` | `scope.zones` with two entries is rejected for this type and still ignored for the others. |

## 6. Rollout

| Step | Content | Gate |
|---|---|---|
| 1 | Properties, size/OS mapping tables, zone validation | Table-driven tests |
| 2 | AWS: instance, security group, instance profile, IMDSv2, AMI lookup | `WithMocks` assertions |
| 3 | AWS scheduling composition (RFC 012) | Same |
| 4 | GCP: instance, Shielded VM, OS Login, native instance schedule | Same |
| 5 | Azure: instance, Trusted Launch, generated key, runbook `deallocate` | Same |
| 6 | `docs/cli.md`, `docs/architecture.md`, `docs/openapi.yaml`, `docs/dependency-licenses.md`, coverage floors | CI green, `gosec`, `govulncheck` |

Steps 1–3 are independently useful: after step 3 a user can deploy and schedule
a hardened VM on AWS, which is the gap §1 opens with.

## 7. Open Questions

1. **An explicit SKU escape hatch?** A `machine_type` property would let a user
   ask for `c7g.4xlarge` when `large` is not enough, at the cost of a
   provider-specific value in a cloud-agnostic document. Proposed answer:
   **not in this RFC** — ship the three sizes, and let a real request for the
   escape hatch justify it rather than speculation.
2. **Provider-specific images.** `amazon-linux-2023` and Azure's own images are
   excluded so that `os` never means "valid depending on provider". Proposed
   answer: **keep the enum portable**, revisit if asked for.
3. **`container_service` and `provider: "agnostic"`.** The other two values the
   system prompt advertises and nothing implements. Out of scope here; each
   deserves its own RFC, and agnostic resolution in particular is a design
   decision (RFC 001 §5, open question 2) rather than an implementation.
4. **Should `public_ip: true` be refused when `scope.environment` looks like
   production?** Consistent with RFC 011 §7.2 and RFC 012 §11.1, proposed answer
   is **no**: no implicit behavior keyed on a free-form string. The confirmation
   gate is the control.

## 8. Implementation notes

Deviations and findings worth recording against the plan above.

- **GCP needed a deny rule the RFC did not anticipate.** §2.2 listed
  "firewall: no ingress" as though absence were sufficient. It is not: the
  `default` network ships `default-allow-ssh`, permitting port 22 from
  anywhere, and GCP firewall rules are allow-only. The instance now carries a
  network tag and an explicit **deny** rule at priority 0, which is what
  actually closes it. Had this shipped as drafted, every GCP VM would have been
  reachable on SSH from the internet the moment it had a public address.

- **Azure needed a whole network.** AWS and GCP both have a default VPC to land
  in; Azure has none, so the provider declares a VNet, subnet, NSG, interface
  and association of its own. The upside is that the perimeter is entirely
  ours — an NSG with no custom rules still carries Azure's `DenyAllInBound`.

- **The scheduling code was generalized rather than duplicated.** Both the AWS
  and Azure schedule declarations were RDS/Flexible-Server specific. Each now
  takes a small target descriptor (`scheduleTarget` on AWS, `powerTarget` on
  Azure) carrying the three things that actually differ — which API to call,
  with what payload, and which IAM actions that needs — so the role, trust
  policy and per-rule machinery are written once. On Azure that descriptor is
  also where `stopAction` lives, which is how `deallocate` reaches the runbook.

- **The zone sentinel is `compute.ErrMultipleZones`**, not
  `ErrMultipleZonesNotSupported` as §2.3 named it: it lives in the shared
  package, where the `compute.` qualifier already carries the context.

- **`providerOpts` now returns invoke options too.** The AMI lookup needs
  `pulumi.InvokeOption`, and building the explicit provider twice would
  register two resources under one Pulumi name. One instance, both flavours.

- **Two pre-existing tests used `compute_instance` as their "unsupported
  type" fixture** and started failing for the right reason. They now use
  `container_service`, which remains advertised and unimplemented (§7.3).

- **`pulumi-tls` added, Apache-2.0**, recorded as an amendment in
  `docs/dependency-licenses.md`. No new licence class enters the graph.

- **Coverage.** `internal/provider/compute` 100%, and the three provider
  packages moved to 65.6% / 71.0% / 71.2% (AWS / GCP / Azure). The AWS figure
  is the lowest because its remaining uncovered statements are the Pulumi
  Automation API surface, which needs a live control plane; everything below
  that boundary is asserted through `pulumi.WithMocks`, including the AMI
  lookup, which the mock monitor now answers.

- **Scans.** `gosec` and `govulncheck` both clean, no new suppressions.
