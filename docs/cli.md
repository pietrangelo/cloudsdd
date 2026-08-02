# CloudSDD CLI

`cloudsdd` translates a natural-language description of infrastructure into a
strict SDD Specification, shows you the resulting plan, and applies it after
confirmation.

You describe *what* you want. CloudSDD decides *how* to build it securely — you
do not ask for encryption, private networking, or deletion protection, because
they are applied by default (RFC 011 §2.5).

- Architecture: [architecture.md](architecture.md)
- CLI design: [RFC 006](rfc/006-cli-entrypoint.md)
- Configurable AI providers: [RFC 010](rfc/010-configurable-ai-providers.md)
- Hardening and policy enforcement: [RFC 011](rfc/011-provider-hardening-and-test-coverage.md)

## Install

```sh
go build -o cloudsdd ./cmd/cloudsdd
```

Requires the [`pulumi` CLI](https://www.pulumi.com/docs/install/) on `PATH`: the
Automation API shells out to it even for an inline Go program.

## Commands

| Command | What it does |
|---|---|
| `cloudsdd deploy "<description>"` | Translate, plan, confirm, apply |
| `cloudsdd destroy "<description>"` | Translate, plan, confirm, destroy |
| `cloudsdd help` | Usage |

### Flags

| Flag | Applies to | Meaning |
|---|---|---|
| `-y`, `--yes` | `deploy`, `destroy` | Skip the interactive confirmation. Required for non-interactive use — without a TTY and without this flag the run is declined rather than guessed at. |
| `--schedule-start` | `deploy` | Power-on time for scheduled resources, `HH:MM`. |
| `--schedule-stop` | `deploy` | Power-off time for scheduled resources, `HH:MM`. |
| `--schedule-timezone` | `deploy` | IANA zone the schedule runs in, e.g. `Europe/Rome`. |
| `--schedule-days` | `deploy` | Comma-separated days the resources are up, e.g. `mon,tue,wed,thu,fri`. Defaults to Monday–Friday. |
| `--no-schedule` | `deploy` | Drop any power schedule the translation proposed. |

The scheduling flags are absent from `destroy` on purpose: destroying a resource
removes its schedules along with it, so there is nothing to configure.

## Environment

| Variable | Required | Purpose |
|---|---|---|
| `CLOUDSDD_PULUMI_PASSPHRASE` | Yes | Encrypts the local Pulumi state (secrets provider). Only needed for the providers your Specification actually targets — RFC 011 §2.8 made provider construction lazy, so an AWS-only deploy no longer demands GCP and Azure configuration. |
| `ANTHROPIC_API_KEY` | If `ai.provider: anthropic` | Anthropic API key. |
| `OPENAI_API_KEY` | If `ai.provider: openai` | OpenAI API key. |
| `CLOUDSDD_STATE_DIR` | No | Pulumi state directory. Defaults to `~/.cloudsdd/state`. |
| `CLOUDSDD_HOME` | No | Configuration and ledger directory. Defaults to `~/.cloudsdd`. |
| `CLOUDSDD_OLLAMA_ENDPOINT` | No | Ollama generate endpoint. Defaults to `http://localhost:11434/api/generate`. |
| `ANTHROPIC_BASE_URL`, `OPENAI_BASE_URL` | No | Override the API base URL (local gateways, proxies). |

Cloud credentials are read from each provider's standard local chain (AWS
profile/env, `gcloud` ADC, Azure CLI). CloudSDD never accepts credentials in a
Specification: a property whose name looks like one is rejected outright
(RFC 011 §2.1).

## Configuration

`~/.cloudsdd/config.yaml`, created with `0600` permissions on first run:

```yaml
ai:
  provider: anthropic       # anthropic | openai | ollama
  model: claude-opus-5
```

Defaults per provider: `claude-opus-5` (anthropic), `gpt-4o` (openai),
`llama3` (ollama).

## Usage

```sh
export CLOUDSDD_PULUMI_PASSPHRASE='…'
export ANTHROPIC_API_KEY='…'

cloudsdd deploy "a private encrypted S3 bucket called app-assets in eu-central-1"
```

```
Translating prompt to SDD Specification...

--- Translated Specification (intent: deploy) ---
{
  "sdd_version": "1.0",
  "intent": "deploy",
  "resources": [
    {
      "id": "app-assets",
      "type": "object_storage",
      "provider": "aws",
      "scope": { "region": "eu-central-1" },
      "properties": { "bucket_name": "app-assets" }
    }
  ]
}
--------------------------------------------
Planning infrastructure changes...
Resource: app-assets (region: eu-central-1) -> action: create

Do you want to apply these changes? (y/N): y
Applying changes...
Apply completed successfully:
- app-assets (region: eu-central-1): applied
```

Note the Specification does not mention encryption or public-access blocking:
both are on by default. You only spell out a security property to *relax* it.

### Non-interactive

```sh
cloudsdd deploy --yes "a postgres 15 database in eu-central-1"
```

## Databases

`relational_database` is encrypted at rest with 7-day backups, protected
against deletion, and has no public endpoint, on all three clouds
(RFC 015).

| Property | Values |
|---|---|
| `engine` | `postgres`, `mysql` — plus `mariadb` on AWS |
| `version` | engine version, e.g. `15` or `8.0` |
| `high_availability` | default `false`; multi-AZ / zone-redundant, with geo-redundant backups |
| `deletion_protection` | default `true` |
| `skip_final_snapshot` | default `false`, AWS only |

**"No public endpoint" is not the same as reachable.** Each cloud gets
there differently, and only one of the three leaves you a network to
connect from:

| Provider | Mechanism | What can reach it |
|---|---|---|
| AWS | A VPC of CloudSDD's own, one per environment and region, with a security group admitting only the engine's port from that VPC | Anything in the same environment — and nothing in another one |
| Azure | A VNet per environment and region, with a subnet delegated per engine and a shared private DNS zone | Anything in the same environment |
| GCP | A custom-mode VPC per environment and region, peered with `servicenetworking` so Cloud SQL has a private address at all | Anything in the same environment |

All three now put a scope's resources on one network, so a VM and a
database in the same `environment` and region can reach each other, and
nothing in another environment can reach either.

The network is created before the first resource in an environment and
removed after the last one:

```
$ cloudsdd destroy "remove the app-db database"
...
Destroy completed successfully:
- app-db (region: eu-central-1): destroyed
- removed the aws network for dev/eu-central-1 (nothing left in it)
```

It is removed **only** when the ledger shows the environment empty. If
the ledger cannot be read, or anything is still recorded there, the
network stays: an empty network costs a little, and the alternative
mistake cuts running resources off from everything they talk to.
Until the remaining steps land, treat these databases as
provisioned-and-private rather than connected, and know that Azure at
least gives you a VNet to peer or attach to.

### Address planning

Once scope networks exist, each `(account, environment, region)` gets its
own, with a range derived deterministically so that two of them never
overlap — overlapping ranges are harmless until the day you try to peer
them, and then the only fix is rebuilding both. Accounts never share a
network, and two accounts holding the same range is fine rather than a
conflict, because CloudSDD creates no route between them.

You need to configure nothing. If you have an existing address plan:

```json
"policies": {
  "network": {
    "base_cidr": "172.20.0.0/14",
    "scopes": { "prod::live::eu-central-1": "172.20.16.0/20" }
  }
}
```

`base_cidr` confines derivation to the block you set aside; `scopes` pins
an individual scope by hand. If two scopes in one account ever resolve to
the same range, CloudSDD refuses to deploy and names both plus the pin
that resolves it, rather than quietly building overlapping networks.

Two Azure notes:

- Deletion protection is enforced twice, because neither half is
  sufficient alone. A `CanNotDelete` **management lock** stops deletion
  through the portal or `az`, which never consult CloudSDD's state; and
  Pulumi's **protect** flag stops `cloudsdd destroy`, which would
  otherwise remove the lock first and the server second. Setting
  `deletion_protection: false` and applying clears both — the same
  two-step teardown AWS already requires.
- Creating that lock needs `Microsoft.Authorization/locks/write`, held by
  **Owner** and **User Access Administrator** but **not by Contributor**.
  On a Contributor-only identity the deploy fails with an explicit error
  rather than provisioning a database you were told is protected and is
  not. Deploy with `deletion_protection: false` if that is what you want.

### Destroying

```sh
cloudsdd destroy "remove the app-assets bucket"
```

`destroy` refuses to run unless the translated Specification declares
`intent: "destroy"`. Earlier versions overwrote the intent after translation,
which silently hid a translator that had misread the request (RFC 011 §2.4).
If you see:

```
Error: refusing to destroy: the translated specification declares intent "deploy".
```

rephrase so the intent is unambiguous — that message means the model did not
understand you as asking for a teardown.

## Secure defaults

Applied without being asked (RFC 011 §2.5). Each can be relaxed only by saying
so explicitly.

| Resource | Default | Relax with |
|---|---|---|
| `object_storage` | Encryption at rest on | `encryption: false` (AWS only; GCP and Azure cannot disable it and will reject the request) |
| `object_storage` | Public access blocked | `block_public_access: false` |
| `object_storage` | Bucket destroy refused when non-empty (GCP) | `force_destroy: true` |
| `relational_database` | Deletion protection on | `deletion_protection: false` |
| `relational_database` | Final snapshot taken on destroy (AWS) | `skip_final_snapshot: true` |
| `relational_database` | No public endpoint | — |
| `relational_database` | Encrypted at rest, 7-day backups | — |
| `compute_instance` | No public address | `public_ip: true` (opens no port) |
| `compute_instance` | No inbound firewall rule at all | — |
| `compute_instance` | Encrypted root disk | — |
| `compute_instance` | IMDSv2 required, hop limit 1 (AWS) | — |
| `compute_instance` | Verified boot and vTPM (GCP Shielded VM, Azure Trusted Launch) | — |
| `compute_instance` | No SSH key; access via Session Manager, OS Login or AAD login | — |

## Compute instances

```
cloudsdd deploy "a small ubuntu build agent in eu-central-1, off outside working hours"
```

```json
{
  "id": "build-agent",
  "type": "compute_instance",
  "provider": "aws",
  "scope": { "region": "eu-central-1", "zones": ["eu-central-1a"] },
  "properties": { "size": "small", "os": "ubuntu-22.04", "disk_size_gb": 30 }
}
```

| Property | Values |
|---|---|
| `size` | `small`, `medium`, `large` — mapped per provider (`t3.small`, `e2-small`, `Standard_B1ms`, …) |
| `os` | `ubuntu-22.04`, `ubuntu-24.04`, `debian-12` — only images that exist on all three clouds |
| `disk_size_gb` | 8–1024, default 20 |
| `public_ip` | default `false`; `true` attaches an address but opens no port |

`scope.zones` accepts at most one entry: a VM occupies one availability zone,
and taking the first of several would hand you infrastructure that does not
match what you wrote.

**There is no SSH key, AMI, or machine-type property.** Access goes through the
cloud's own IAM-authenticated session service — Session Manager on AWS, OS Login
on GCP, the AAD login extension on Azure — none of which needs an inbound port
or a public address, and all of which leave an audit trail. Azure's API insists
on an admin key, so one is generated during the deploy and its private half
stays in the encrypted Pulumi state; nothing logs in with it.

Two provider notes worth knowing:

- On Azure, `encryption_at_host` requires the `Microsoft.Compute/EncryptionAtHost`
  feature to be registered on the subscription. If it is not, the deploy fails
  with an explicit error rather than quietly provisioning less protection.
- On GCP the instance carries an explicit deny-ingress rule at priority 0. The
  `default` network ships `default-allow-ssh`, which permits port 22 from
  anywhere, and GCP firewall rules are allow-only — so nothing less actually
  closes the machine.

## Container services

```
$ cloudsdd deploy "run ghcr.io/acme/api@sha256:9f2c… on port 8080 in eu-central-1"
```

Implemented on all three clouds: AWS (ECS on Fargate), GCP (Cloud Run) and
Azure (Container Apps). The service lands in the environment's own network,
beside the database it was deployed to talk to, and pulls its image through
that network's NAT gateway.

```json
{
  "id": "api",
  "type": "container_service",
  "provider": "agnostic",
  "scope": { "region": "eu-central-1", "environment": "prod" },
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

`image` and `port` are required. `size` is `small`/`medium`/`large`, mapped to
each platform's CPU and memory pairs. `replicas` defaults to 1 and is capped at
10.

**The image is the only property that decides what code runs**, so it is the
one this tool is strictest about — see [`allowed_registries`](#allowed_registries).

### Reachability

`public` defaults to **false**, and a private service is reachable only from its
own environment's network. That is the deliberate choice for the one resource
type that runs arbitrary code.

`public: true` means HTTPS, never a raw open port. What that takes differs:

| | AWS | GCP | Azure |
|---|---|---|---|
| Endpoint | An internet-facing ALB in the environment's public subnets | The built-in `*.run.app` endpoint | The built-in `*.azurecontainerapps.io` endpoint |
| Certificate | ACM, validated through DNS | Google-managed | Azure-managed |
| `domain` | **Required** | Optional | Optional |
| Plain HTTP | Redirected (301), never served | Redirected by Cloud Run | Redirected by the managed ingress |

The asymmetry is not an oversight. ACM will not issue a certificate for a load
balancer's own `*.elb.amazonaws.com` name and AWS has no equivalent of
`*.run.app`, so a public service on AWS with no hostname could only be served
over plain HTTP. CloudSDD refuses the Specification instead:

```
Error: aws: a public container_service requires `domain`; AWS cannot issue a
certificate for a load balancer's own name
```

Where a `domain` is given, it must be served from a hosted zone in the target
account — **Route 53** on AWS, **Azure DNS** on Azure. The managed certificate
is issued only once the verification records resolve, so CloudSDD creates them:
on AWS the ACM validation record plus an alias record pointing the domain at
the balancer, on Azure a CNAME to the app plus the `asuid.` TXT record. Without
the resolving record the certificate would be valid and the hostname would
point nowhere.

A `domain` without `public: true` is refused on every provider: a hostname on
something nothing outside can reach is a request that would not be honoured.

### What you get without asking

- A dedicated identity per service with **no permissions attached**. On AWS
  that is a task role separate from the execution role the ECS agent uses to
  pull the image; on GCP a service account declared explicitly, because Cloud
  Run's fallback is the default compute account, which carries Editor on the
  whole project; on Azure a user-assigned managed identity with no role
  assignments.
- The container's port reachable **only from the load balancer**, by security
  group rather than by address range — anything else in the same subnet is
  still shut out.
- TLS 1.2 and above on AWS. The default ALB policy still admits TLS 1.0.
- Plain HTTP refused on Azure. `allowInsecureConnections` defaults to *true*
  there, which would serve HTTP alongside HTTPS instead of redirecting.
- No `env` property to paste a credential into. Configuration injection needs a
  secrets story and will get its own RFC.
- Logs retained 30 days on AWS, so a chatty service does not accumulate a bill
  nobody chose.

`replicas: 0` — deployed and running nothing — works on AWS and Azure. It is
refused on GCP: Cloud Run reads a zero ceiling as *unset* and would apply its
own default, uncapping the service rather than stopping it. Cloud Run already
scales to zero between requests, so nothing is lost but the ability to say "and
never scale up". The same reasoning is why a power schedule does not apply to a
Cloud Run service — see [What can be scheduled](#what-can-be-scheduled).

## Choosing a cloud

A resource may declare `"provider": "agnostic"` and let CloudSDD decide.

```
$ cloudsdd deploy "a bucket in europe-west1"
...
Resolved agnostic resources:
- assets -> gcp (the only provider that can express it)
```

**Resolution is a declared rule, never a guess.** A candidate is a provider
whose validation accepts the resource — the same check that would run if you
had named it — so a cloud can never be chosen for a resource it would then
reject.

In practice the region usually decides on its own, because the three formats
are mutually exclusive:

| Provider | Shape | Example |
|---|---|---|
| AWS | `xx-name-N` | `eu-central-1` |
| GCP | `name-nameN` | `europe-west1` |
| Azure | one lowercase word | `westeurope` |

So `agnostic` plus `eu-central-1` has exactly one candidate and needs no
configuration. It also means writing `agnostic` with a provider-specific region
is not portability — it is that provider spelled indirectly. Portable region
names are a separate, larger feature (RFC 014 §7.1).

When more than one cloud could serve a resource, CloudSDD looks for an explicit
preference and **refuses to choose** if it finds none:

1. `policies.provider_preference` in the Specification, an ordered list;
2. `defaults.provider` in `~/.cloudsdd/config.yaml`;
3. otherwise an error naming every candidate.

```yaml
# ~/.cloudsdd/config.yaml
defaults:
  provider: aws
```

The refusal is deliberate. A Specification that resolved to AWS in review and
Azure in production would be a change of blast radius nobody approved.

A provider that cannot be constructed — no passphrase, no credentials — is
excluded from candidacy and **reported**:

```
Note: azure is not available as a candidate (CLOUDSDD_PULUMI_PASSPHRASE must be set)
```

Silently dropping it would make the same Specification resolve differently on a
colleague's machine with no way to see why. A provider you named *explicitly*
still fails hard: you asked for it specifically.

## Policies

```json
"policies": { "allowed_regions": ["eu-central-1"] }
```

`allowed_regions` is enforced by the Engine for every provider (RFC 011 §2.3).
`max_cost_monthly` was removed in RFC 011 §2.9: it was validated but never
enforced, so it read as a guarantee and provided none. Real cost enforcement
needs a pricing model and is deferred to its own RFC.

### `allowed_registries`

```json
"policies": { "allowed_registries": ["ghcr.io"] }
```

Which container registries a `container_service` may pull from (RFC 017 §2.4).
It is enforced by the Engine, alongside `allowed_regions`, so no provider can
omit it.

Leaving it out is not the permissive setting. Two rules apply, on two
independent axes:

- **Mutability.** With no allowlist, **every image must be pinned to a digest**
  (`ghcr.io/acme/api@sha256:…`). A tag can be repointed after you reviewed the
  plan, so what runs would not be what you approved. Naming a registry here is
  how you take responsibility for what its tags point at — and then a tag is
  accepted from it.
- **Origin.** When the list is present it constrains *every* image, digest or
  not. An allowlist something can step around is not a policy.

`:latest` is refused in every combination, with or without an allowlist, pinned
or not: nothing that reads "deploy whatever is newest, forever" belongs in a
reviewed artifact. An image with no tag at all is the same thing spelled
differently, and is refused the same way.

## Power scheduling

Ask for it in the prompt, and the environment is powered off outside working
hours (RFC 012). Nothing is scheduled unless you ask.

```
$ cloudsdd deploy "a dev postgres database in eu-central-1, shut it down outside working hours"

This deployment includes a power schedule: app-db will be shut down outside working hours.
  Start time (HH:MM): 08:00
  Stop time (HH:MM): 19:00
  Timezone [Europe/Rome]:
  Days [mon,tue,wed,thu,fri]:

...
Power schedule:
- app-db: Mon-Fri 08:00 -> 19:00 (Europe/Rome), weekend off
```

**The times are asked for, never inferred.** The translator is instructed to
emit the *intent* to schedule and nothing else; a start time it guessed wrong
would be an inconvenience, a stop time it guessed wrong is an outage. In
non-interactive use (`--yes`, or no TTY) an unresolved schedule is a hard error
naming the flags that would resolve it.

The schedule is provisioned as cloud-native resources in the target account —
EventBridge Scheduler on AWS, Cloud Scheduler on GCP, Automation on Azure — so
it keeps working with no `cloudsdd` process running anywhere.

### Schema

Declared once in `policies`, inherited by every schedulable resource:

```json
"policies": {
  "schedule": {
    "enabled": true,
    "timezone": "Europe/Rome",
    "start": "08:00",
    "stop": "19:00",
    "days": ["mon", "tue", "wed", "thu", "fri"],
    "exceptions": [
      { "from": "2026-09-12", "to": "2026-09-14", "mode": "always_on",  "reason": "release weekend" },
      { "from": "2026-12-24", "to": "2027-01-06", "mode": "always_off", "reason": "company shutdown" }
    ]
  }
}
```

- `days` absent means Monday–Friday, which leaves the weekend off: no start
  fires on Saturday or Sunday, and Friday's stop leaves the environment down.
- A resource can override the policy or opt out of it entirely with
  `"schedule": {"enabled": false}` — the reviewable way to keep production up.
- `start` later than `stop` is an overnight window and is accepted.
- The timezone must be an IANA zone name. A fixed offset is rejected: it is an
  hour wrong for half the year.
- Exception windows may not overlap. Ambiguity is an error rather than a
  precedence rule you would have to guess at.

### What can be scheduled

`relational_database`, `compute_instance` and `container_service` can be
scheduled. An **explicit** schedule on an object store or an IAM role is an
error; one **inherited** from `policies` is skipped and reported in the plan, so
a Specification can hold both a bucket and a database.

**One exception is provider-specific**: a `container_service` on **GCP** cannot
be scheduled, because Cloud Run bills per request and idles to zero between them
— there is no running state to switch off, and the saving a schedule exists to
deliver is already unconditional. The same explicit/inherited rule applies:

```
$ cloudsdd deploy "run api on cloud run, shut it down outside working hours"
Error: engine: resource "api" is a "container_service" on "gcp":
resource type does not support a power schedule
```

while an environment-wide `policies.schedule` alongside a Cloud Run service is
accepted and reported honestly:

```
Power schedule:
- app-db: on Mon-Fri from 08:00 to 19:00 (Europe/Rome)
- api: not applicable to container_service on gcp, this resource stays up
```

### Provider differences

| Provider | Mechanism | Exception windows |
|---|---|---|
| AWS | EventBridge Scheduler, universal RDS, EC2 and ECS targets | Supported |
| Azure | Automation account, runbook and schedules | Supported |
| GCP, `relational_database` | Cloud Scheduler against the Cloud SQL Admin API | **Rejected** — a Cloud Scheduler job has no validity period |
| GCP, `compute_instance` | Native Compute Engine instance schedule | **Rejected** — an instance accepts one policy, with one validity interval |
| GCP, `container_service` | — | **Not scheduled at all**, see above |

A container service has no power state, only a replica count, so "off" is zero
replicas rather than a stopped task: on AWS both rules call `ecs:UpdateService`
with different desired counts, and on Azure the app's own `start` and `stop`
actions release the replicas rather than leaving them allocated.

The GCP refusal is deliberate. Silently dropping a window would leave an
environment running through a shutdown you believed you had scheduled, and the
failure would reach you as an invoice rather than an error.

Two details worth knowing. An `always_off` window issues a **daily** stop, not a
single one, because AWS automatically restarts an RDS instance that has been
stopped for more than seven days. And on Azure a scheduled stop **deallocates**
the machine rather than stopping it: an Azure VM in the `Stopped` state still
bills for compute, so the other verb would run correctly and save nothing.

## The ledger

`~/.cloudsdd/ledger.json` records what has been deployed, and is supplied to the
translator so it can resolve references to existing infrastructure.

- Entries are keyed by `(account, environment, region, id)`, so the same
  resource ID in `dev` and `prod` stays two distinct records (RFC 011 §2.7).
- Only resources that actually applied are recorded, so a partially-failed run
  still tracks what was created.
- Writes are atomic and guarded by a cross-process lockfile, so concurrent
  `cloudsdd` invocations cannot lose entries.
- It is passed to the model as **untrusted data**, explicitly delimited and
  size-capped, and everything the model returns is still gated by strict
  schema validation (RFC 011 §4).

## Exit codes

| Code | Meaning |
|---|---|
| `0` | Success, or the plan was declined at the confirmation prompt with no changes made |
| `1` | Any error: translation, validation, policy rejection, intent mismatch, or an apply/destroy failure |

On a partial failure the CLI prints what completed before the error and records
it in the ledger, so a retry does not re-create resources that already exist.

## Troubleshooting

| Symptom | Cause |
|---|---|
| `CLOUDSDD_PULUMI_PASSPHRASE is required` | Unset. Needed by every provider your Specification targets. |
| `refusing to deploy: … declares intent "destroy"` | The translator read your prompt as a teardown. Rephrase, or use `destroy`. |
| `region … not in allowed_regions` | The Specification's `policies.allowed_regions` excludes the requested region. |
| `container image must be pinned to a digest …` | The image carries a tag and its registry is not in `policies.allowed_registries`. Pin it (`@sha256:…`) or allow-list the registry. |
| `container image must not use the \`latest\` tag` | `:latest`, or no tag at all. Name a version or a digest; there is no policy that permits it. |
| `container image registry not in allowed_registries` | The image comes from a registry the Specification does not list. A digest does not exempt it. |
| `a public container_service requires \`domain\`` | AWS only. ACM cannot certify a load balancer's own name, so a hostname is needed to serve HTTPS. |
| `\`domain\` requires \`public: true\`` | A hostname on a service nothing outside can reach. Drop it, or make the service public. |
| `no public Route 53 hosted zone … found` | The domain is not served from a Route 53 zone in this account, so the certificate cannot be validated through DNS. |
| `Cloud Run cannot be pinned to zero replicas` | GCP only. Cloud Run reads a zero ceiling as unset; it already scales to zero between requests. |
| `unknown or malformed property` | The translator produced a property the provider does not support. Since RFC 011 these are rejected instead of silently dropped, so the message names what would have been ignored. |
| `property key … looks like a credential` | A credential was placed in the Specification. Credentials come from the local environment only. |
| `encryption cannot be disabled on …` | GCP and Azure encrypt at rest unconditionally; the request is refused rather than quietly ignored. |
| `failed to declare the deletion lock … requires Microsoft.Authorization/locks/write` | The Azure identity is Contributor, which cannot create management locks. Use Owner or User Access Administrator, or deploy with `deletion_protection: false`. |
| `resource … is protected` on destroy | Deletion protection is on. Set `deletion_protection: false`, apply, then destroy. |
| `ledger is locked by another process` | Another `cloudsdd` run holds the ledger. Wait, or remove `~/.cloudsdd/ledger.lock` if no run is active. |
| Operation cancelled with no prompt shown | Non-interactive invocation without `--yes`. |
