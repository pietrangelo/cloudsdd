# API CloudSDD

## Stato: non implementata

Nessun layer HTTP esiste ancora nel codice (`internal/api` non è stato
creato). La RFC 001 (`docs/rfc/001-core-architecture-and-json-schema.md`)
copre esclusivamente il core del motore (`internal/spec`,
`internal/provider`, `internal/engine`): l'esposizione via API HTTP è
esplicitamente fuori scope (RFC 001 §4) e richiederà una RFC dedicata,
che dovrà coprire almeno:

- routing e scelta tra `net/http` nativo e Chi (CLAUDE.md, standard Go);
- autenticazione/identity propagata via `context.Context` (necessaria per
  la prevenzione BOLA descritta in RFC 001 §3);
- rate limiting (`golang.org/x/time/rate`) a livello di middleware;
- limiti su dimensione payload e numero massimo di risorse per richiesta
  (prevenzione Unrestricted Resource Consumption);
- mapping tra endpoint HTTP e le operazioni già definite in
  `engine.Engine` (`Validate`, `Plan`, `Apply`).

## Uso programmatico attuale (Go, nessuna API HTTP)

Fino all'introduzione dell'API HTTP, il motore è utilizzabile solo come
libreria Go interna al modulo:

```go
import (
    "cloudsdd/internal/engine"
    "cloudsdd/internal/provider"
    "cloudsdd/internal/spec"
)

s, err := spec.ParseAndValidate(r) // r: io.Reader col JSON della Specifica
if err != nil {
    // errore di parsing o di validazione di dominio
}

e := engine.New(map[spec.Provider]provider.CloudProvider{
    // spec.ProviderAWS: awsProvider, // nessuna implementazione concreta esiste ancora
})

diffs, err := e.Plan(ctx, *s)
results, err := e.Apply(ctx, *s)
```

Vedi `docs/openapi.yaml` per lo schema del solo payload `Specification`
(componente riutilizzabile quando l'API verrà definita).
