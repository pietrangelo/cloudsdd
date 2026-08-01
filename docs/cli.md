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
| AWS | `publicly_accessible: false` | Anything in the account's default VPC |
| Azure | VNet integration: delegated subnet + private DNS zone | Anything in the database's own VNet, which today contains only the database |
| GCP | Private IP only (`ipv4_enabled: false`) | Nothing yet — a private IP needs a VPC with private services access, which CloudSDD does not model |

Deciding what shares a network is a feature CloudSDD does not have; it
needs its own RFC (RFC 005 §5, RFC 015 §1.1). Until then, treat these
databases as provisioned-and-private rather than connected, and know that
Azure at least gives you a VNet to peer or attach to.

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

Only `relational_database` has a power state today. An **explicit** schedule on
an object store or an IAM role is an error; one **inherited** from `policies` is
skipped and reported in the plan, so a Specification can hold both a bucket and
a database.

### Provider differences

| Provider | Mechanism | Exception windows |
|---|---|---|
| AWS | EventBridge Scheduler, universal RDS and EC2 targets | Supported |
| Azure | Automation account, runbook and schedules | Supported |
| GCP, `relational_database` | Cloud Scheduler against the Cloud SQL Admin API | **Rejected** — a Cloud Scheduler job has no validity period |
| GCP, `compute_instance` | Native Compute Engine instance schedule | **Rejected** — an instance accepts one policy, with one validity interval |

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
| `unknown or malformed property` | The translator produced a property the provider does not support. Since RFC 011 these are rejected instead of silently dropped, so the message names what would have been ignored. |
| `property key … looks like a credential` | A credential was placed in the Specification. Credentials come from the local environment only. |
| `encryption cannot be disabled on …` | GCP and Azure encrypt at rest unconditionally; the request is refused rather than quietly ignored. |
| `failed to declare the deletion lock … requires Microsoft.Authorization/locks/write` | The Azure identity is Contributor, which cannot create management locks. Use Owner or User Access Administrator, or deploy with `deletion_protection: false`. |
| `resource … is protected` on destroy | Deletion protection is on. Set `deletion_protection: false`, apply, then destroy. |
| `ledger is locked by another process` | Another `cloudsdd` run holds the ledger. Wait, or remove `~/.cloudsdd/ledger.lock` if no run is active. |
| Operation cancelled with no prompt shown | Non-interactive invocation without `--yes`. |
