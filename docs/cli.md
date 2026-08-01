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
| `relational_database` | Not publicly reachable | — |
| `relational_database` | Encrypted at rest, 7-day backups | — |

## Policies

```json
"policies": { "allowed_regions": ["eu-central-1"] }
```

`allowed_regions` is enforced by the Engine for every provider (RFC 011 §2.3).
`max_cost_monthly` was removed in RFC 011 §2.9: it was validated but never
enforced, so it read as a guarantee and provided none. Real cost enforcement
needs a pricing model and is deferred to its own RFC.

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
| `ledger is locked by another process` | Another `cloudsdd` run holds the ledger. Wait, or remove `~/.cloudsdd/ledger.lock` if no run is active. |
| Operation cancelled with no prompt shown | Non-interactive invocation without `--yes`. |
