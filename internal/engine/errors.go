package engine

import "errors"

// ErrProviderNotFound indica che nessun CloudProvider è registrato per il
// provider richiesto da una risorsa.
var ErrProviderNotFound = errors.New("engine: provider not registered")

// ErrAgnosticResolutionNotImplemented indica che la risorsa richiede
// provider "agnostic" ma la policy di risoluzione automatica non è ancora
// stata implementata (RFC 001 §5, domanda aperta 2).
var ErrAgnosticResolutionNotImplemented = errors.New("engine: agnostic provider resolution not implemented")
