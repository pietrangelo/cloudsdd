# Architettura CloudSDD

> Stato corrente del sistema. Le decisioni di design sono tracciate nelle RFC
> in `docs/rfc/`; questo documento riflette cosa è stato **effettivamente
> implementato**, non le proposte.

## Panoramica

CloudSDD converte una Specifica SDD (JSON) in operazioni su provider cloud,
tramite un motore Go cloud-agnostico. Il flusso end-to-end previsto è:

```
Richiesta NL ──(non ancora implementato)──► Specifica JSON ──► Engine.Validate
                                                                      │
                                                                      ▼
                                                                Engine.Plan
                                                                      │
                                                                      ▼
                                                                Engine.Apply
```

Solo la parte "Specifica JSON → Engine" è implementata ad oggi (RFC 001).
La traduzione linguaggio naturale → Specifica e l'API layer HTTP sono fuori
scope e non hanno ancora una RFC dedicata.

## Struttura del modulo

```
cloudsdd/
├── cmd/                    # Entry point eseguibili (vuoto: nessuna RFC li ha ancora definiti)
├── internal/
│   ├── spec/                # Tipi della Specifica, parsing strict, validazione di dominio
│   ├── provider/             # Interfaccia CloudProvider, tipi Diff/Result
│   └── engine/                # Interfaccia Engine e implementazione DefaultEngine
├── pkg/                     # Vuoto: nessun tipo pubblico esposto ancora
└── docs/
    ├── rfc/001-...           # RFC di fondazione (approvata)
    ├── architecture.md        # Questo documento
    ├── api.md                 # Stato dell'API HTTP (non ancora implementata)
    └── openapi.yaml            # Schema OpenAPI (solo componente Specification, no paths)
```

## Pacchetto `internal/spec`

Rappresenta e valida la Specifica SDD (`Specification`, `Resource`, `Policies`).

- **`Parse(io.Reader) (*Specification, error)`**: decodifica JSON strict
  (`json.Decoder.DisallowUnknownFields`) e rifiuta dati JSON residui dopo il
  primo valore. Questo è il primo livello di difesa contro il **Mass
  Assignment**: qualunque campo non previsto dallo schema causa un errore
  esplicito invece di essere ignorato o assegnato silenziosamente.
- **`Validate(*Specification) error`**: secondo livello di validazione,
  tramite `go-playground/validator`, che applica le regole di dominio (tag
  `validate` sulle struct): `sdd_version` fissata a `"1.0"`, `intent`
  vincolato a un enum, almeno una risorsa, `id` risorsa vincolato al pattern
  `^[a-zA-Z0-9_-]{1,63}$` (tag custom `resourceid`), `type` e `provider`
  vincolati a enum, `max_cost_monthly` positivo se presente.
- **`ParseAndValidate`**: combina i due passi.
- `Resource.Properties` resta `map[string]any`: è un contenitore di
  trasporto JSON generico. La decodifica tipizzata e validata delle
  proprietà specifiche per `ResourceType`/provider è responsabilità di ogni
  `CloudProvider` concreto (non ancora implementato).

Il parser è coperto da un fuzz test nativo Go (`FuzzParse`), eseguito contro
input malformati, JSON annidato, campi sconosciuti e input vuoto: l'invariante
verificata è l'assenza di panic, non l'accettazione dell'input.

## Pacchetto `internal/provider`

Definisce il contratto `CloudProvider` che ogni backend concreto (AWS, GCP,
Azure, ...) dovrà implementare in RFC future:

```go
type CloudProvider interface {
    Name() string
    Validate(ctx context.Context, r spec.Resource) error
    Plan(ctx context.Context, r spec.Resource) (Diff, error)
    Apply(ctx context.Context, r spec.Resource) (Result, error)
    Destroy(ctx context.Context, r spec.Resource) error
}
```

Nessuna implementazione concreta esiste ancora: solo l'interfaccia e i tipi
di supporto `Diff`/`Result`/`Action`/`Status`.

## Pacchetto `internal/engine`

`DefaultEngine` orchestra `Validate`/`Plan`/`Apply` su una `Specification`,
mantenendo un registry immutabile (copiato in `New`) di `CloudProvider`
indicizzato per `spec.Provider`.

Comportamento rilevante:

- `Validate` esegue prima `spec.Validate` (dominio), poi delega la
  validazione per-risorsa al provider risolto.
- `Plan`/`Apply` richiamano `Validate` come precondizione (fail-fast: nessuna
  chiamata a un provider avviene su una Specifica non valida).
- Se `Resource.Provider == "agnostic"`, l'Engine restituisce
  `ErrAgnosticResolutionNotImplemented`: la policy di risoluzione automatica
  del provider è una domanda aperta della RFC 001 (§5, punto 2) e non è
  ancora stata decisa né implementata.
- Provider non registrati producono `ErrProviderNotFound`, sempre
  ispezionabile con `errors.Is`.

## Sicurezza

Misure attive ad oggi (si veda anche `docs/rfc/001-...md` §3):

| Minaccia OWASP API Top 10 | Mitigazione attuale |
|---|---|
| Mass Assignment | Decodifica JSON strict (`DisallowUnknownFields`) + validazione a doppio livello |
| Input ostile / crash del parser | Fuzz test nativo Go su `spec.Parse` |
| BOLA | Non ancora applicabile: non esiste ancora un livello di storage/state multi-tenant |
| Unrestricted Resource Consumption | Non ancora applicabile: non esiste ancora un livello HTTP/API |

Scan eseguiti prima di questo commit: `gosec ./...` (0 issue), `govulncheck
./...` (0 vulnerabilità raggiungibili dal codice; una dipendenza transitiva,
`golang.org/x/text`, è stata aggiornata proattivamente alla versione con
fix).

## Testing

- Test table-driven per `internal/spec` (parsing e validazione) e
  `internal/engine` (orchestrazione, incluso un `mockProvider` di test).
  Copertura: `internal/spec` 90.9%, `internal/engine` 90.7%.
- Fuzz test nativo Go per `spec.Parse`.
- Nessun test di integrazione (`testcontainers-go`) ancora presente: non
  esistono ancora componenti con dipendenze esterne (database, provider
  reali) da testare in questo modo.

## Fuori scope / prossimi passi

Vedi RFC 001 §4 e §5. In sintesi, non ancora implementati: provider
concreti, algoritmo di planning/diffing reale (oggi `Plan`/`Apply` sono
puri pass-through verso il provider), persistenza dello stato, API layer
HTTP, autenticazione/autorizzazione multi-tenant, traduzione NL → Specifica.
