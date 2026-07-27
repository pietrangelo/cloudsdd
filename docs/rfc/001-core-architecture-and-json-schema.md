# RFC 001: Architettura Core e JSON Schema Universale

- **Stato:** Draft — in attesa di approvazione
- **Autore:** Claude (Senior Staff Cloud Platform Engineer, AI-assisted)
- **Data:** 2026-07-27

## 1. Problema

CloudSDD deve trasformare richieste in linguaggio naturale in infrastruttura cloud
effettivamente provisionata, in modo cloud-agnostico, sicuro, ripetibile e
verificabile. Serve quindi:

1. Una **rappresentazione intermedia formale** (la "Specifica") che catturi
   l'intento dell'utente in modo non ambiguo, indipendente dal provider cloud.
2. Un **motore Go** capace di:
   - validare la Specifica contro uno schema rigoroso (difesa da input ostili,
     Mass Assignment, campi non previsti);
   - risolvere la Specifica in risorse concrete su uno o più provider cloud
     (AWS, GCP, Azure, ecc.) tramite un'astrazione comune;
   - applicare lo stato in modo idempotente (create/update/no-op) e riportare
     un piano/diff prima dell'esecuzione, sul modello Terraform/Pulumi.
3. Un confine netto tra "traduzione NL → Specifica" (fuori scope in questa RFC,
   sarà un modulo separato, es. LLM-assisted) e "Specifica → Infrastruttura"
   (il motore Go, oggetto di questa RFC).

Questa RFC copre esclusivamente la **fondazione**: struttura del progetto,
schema JSON universale, e le interfacce core del motore. Non copre ancora
l'implementazione dei singoli provider né la logica di diffing/planning, che
saranno oggetto di RFC successive.

## 2. Architettura Proposta

### 2.1 Struttura del progetto

```
cloudsdd/
├── cmd/                    # Entry point degli eseguibili (es. cmd/cloudsddctl)
├── internal/               # Codice privato dell'applicazione, non importabile
│   ├── spec/               # Parsing, validazione e tipi della Specifica SDD
│   ├── engine/              # Orchestrazione: riceve la Specifica, invoca i provider
│   ├── provider/            # Implementazioni concrete di CloudProvider (aws, gcp, ...)
│   ├── api/                 # HTTP layer (net/http o Chi), routing, middleware
│   └── security/            # Rate limiting, authz (BOLA), auth middleware
├── pkg/                    # Codice pubblico riutilizzabile (client SDK, tipi condivisi)
├── docs/
│   ├── rfc/                 # Le RFC di design (questo documento)
│   ├── architecture.md      # Vista d'insieme sempre aggiornata (creato ad approvazione)
│   └── api.md                # Documentazione API (creato ad approvazione)
└── go.mod
```

Razionale: `internal/` impedisce che i dettagli implementativi (es. un
provider specifico) vengano importati da moduli esterni, mantenendo
l'interfaccia pubblica ridotta a `pkg/`.

### 2.2 Flusso ad alto livello

```
Richiesta NL ──(fuori scope)──► Specifica JSON ──► [1] Validazione
                                                        │
                                                        ▼
                                              [2] Planning (diff stato desiderato
                                                  vs stato attuale)
                                                        │
                                                        ▼
                                              [3] Apply (CloudProvider.Apply)
                                                        │
                                                        ▼
                                              Stato aggiornato + report
```

### 2.3 Interfacce core (design, non implementazione)

Le seguenti interfacce definiscono i contratti principali del motore. Sono
presentate qui come **specifica di design** ai fini dell'approvazione
architetturale — l'implementazione concreta avverrà solo dopo approvazione.

```go
// internal/spec: rappresentazione tipizzata della Specifica SDD.
type Specification struct {
    SDDVersion string     `json:"sdd_version" validate:"required,eq=1.0"`
    Intent     Intent     `json:"intent" validate:"required,oneof=deploy update destroy plan"`
    Resources  []Resource `json:"resources" validate:"required,dive"`
    Policies   Policies   `json:"policies"`
}

type Resource struct {
    ID         string         `json:"id" validate:"required,alphanum_dash"`
    Type       ResourceType   `json:"type" validate:"required"`
    Provider   string         `json:"provider" validate:"required"` // "agnostic" | "aws" | "gcp" | "azure"
    Properties map[string]any `json:"properties" validate:"required"`
}

type Policies struct {
    MaxCostMonthly  *float64 `json:"max_cost_monthly,omitempty" validate:"omitempty,gt=0"`
    AllowedRegions  []string `json:"allowed_regions,omitempty"`
}

// internal/provider: astrazione cloud-agnostica.
type CloudProvider interface {
    Name() string
    Validate(ctx context.Context, r Resource) error
    Plan(ctx context.Context, r Resource) (Diff, error)
    Apply(ctx context.Context, r Resource) (Result, error)
    Destroy(ctx context.Context, r Resource) error
}

// internal/engine: orchestratore che usa CloudProvider senza conoscerne i dettagli.
type Engine interface {
    Validate(ctx context.Context, spec Specification) error
    Plan(ctx context.Context, spec Specification) ([]Diff, error)
    Apply(ctx context.Context, spec Specification) ([]Result, error)
}
```

Punti chiave del design:

- `Resource.Properties` è intenzionalmente `map[string]any` a livello di
  Specifica grezza, ma **ogni provider** definirà una struct tipizzata e
  validata per il proprio `ResourceType` (es. `RelationalDatabaseProps`),
  in modo da bloccare Mass Assignment prima di raggiungere la logica di
  business. La mappa generica esiste solo come contenitore di trasporto
  JSON, mai come tipo su cui si opera direttamente.
- `provider: "agnostic"` delega all'engine la scelta del provider concreto
  in base a `Policies.AllowedRegions` e a criteri di costo/disponibilità
  (logica di risoluzione, dettagliata in una RFC futura).
- Ogni operazione è `context.Context`-aware per supportare timeout,
  cancellazione e propagazione di tenant/identity (necessario per BOLA).

### 2.4 JSON Schema universale (v1.0)

Lo schema seguente è la bozza del JSON Schema (draft 2020-12) che validerà
ogni Specifica in ingresso, in aggiunta alla validazione struct-level via
`go-playground/validator`.

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://cloudsdd.dev/schema/v1.0/specification.json",
  "title": "CloudSDD Specification",
  "type": "object",
  "additionalProperties": false,
  "required": ["sdd_version", "intent", "resources"],
  "properties": {
    "sdd_version": { "const": "1.0" },
    "intent": { "type": "string", "enum": ["deploy", "update", "destroy", "plan"] },
    "resources": {
      "type": "array",
      "minItems": 1,
      "items": { "$ref": "#/$defs/resource" }
    },
    "policies": { "$ref": "#/$defs/policies" }
  },
  "$defs": {
    "resource": {
      "type": "object",
      "additionalProperties": false,
      "required": ["id", "type", "provider", "properties"],
      "properties": {
        "id": { "type": "string", "pattern": "^[a-zA-Z0-9_-]{1,63}$" },
        "type": { "type": "string", "enum": ["relational_database", "object_storage", "compute_instance", "container_service"] },
        "provider": { "type": "string", "enum": ["agnostic", "aws", "gcp", "azure"] },
        "properties": { "type": "object" }
      }
    },
    "policies": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "max_cost_monthly": { "type": "number", "exclusiveMinimum": 0 },
        "allowed_regions": { "type": "array", "items": { "type": "string" } }
      }
    }
  }
}
```

Nota: l'elenco `resources[].type` e `provider` in questa bozza è
intenzionalmente minimo (solo l'esempio da CLAUDE.md); verrà esteso via RFC
dedicate man mano che si aggiungono provider e tipi di risorsa concreti.

## 3. Considerazioni di Sicurezza

- **Validazione a doppio livello**: JSON Schema (struttura/tipi/enum) +
  `go-playground/validator` (regole di dominio) prima che qualunque campo
  raggiunga la logica di business. `additionalProperties: false` a ogni
  livello blocca Mass Assignment sin dal parsing.
- **BOLA**: ogni `Resource.ID` sarà namespaced per tenant a livello di
  storage/state (dettaglio implementativo in RFC futura sul modulo state);
  l'`Engine` non accetterà mai un tenant ID proveniente dal body della
  richiesta, solo dal contesto di autenticazione (`context.Context`).
- **Rate limiting**: previsto in `internal/security` con
  `golang.org/x/time/rate`, applicato a livello di middleware HTTP prima
  che una richiesta raggiunga l'`Engine`.
- **Resource consumption**: limiti su dimensione payload JSON in ingresso e
  su numero massimo di `resources` per Specifica (valore da definire in
  RFC di implementazione dell'API layer).
- **Fuzzing**: il parser della Specifica (`internal/spec`) sarà il primo
  target di `go test -fuzz`, data la sua esposizione diretta a input
  ostile.

## 4. Fuori Scope (per RFC future)

- Traduzione linguaggio naturale → Specifica JSON.
- Implementazione concreta dei provider (AWS/GCP/Azure) via Pulumi Go SDK.
- Algoritmo di diffing/planning e gestione dello state file.
- Autenticazione/autorizzazione (schema tenant, JWT/OIDC).
- API layer HTTP (routing, middleware, rate limiting concreto).

## 5. Domande Aperte

1. Lo state (stato attuale delle risorse applicate) verrà persistito
   localmente, su backend remoto (es. S3-compatible), o entrambi
   configurabili?
2. Il `provider: "agnostic"` richiede una policy di risoluzione: preferenza
   utente esplicita, costo minimo, o prima regione disponibile in
   `allowed_regions`?
3. Versionamento dello schema: `sdd_version` bloccante (`const: "1.0"`) o
   supporto multi-versione con migrazione automatica fin da subito?
