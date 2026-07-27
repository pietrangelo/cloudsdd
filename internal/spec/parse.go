package spec

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ErrTrailingData indica che il payload contiene dati oltre il primo valore JSON valido.
var ErrTrailingData = errors.New("spec: unexpected trailing data after JSON value")

// Parse decodifica una Specifica da r in modo strict: campi JSON non
// previsti nello schema causano un errore invece di essere silenziosamente
// ignorati o assegnati (difesa da Mass Assignment, RFC 001 §3).
//
// Parse non esegue alcuna validazione di dominio: chiamare Validate (o usare
// ParseAndValidate) prima di considerare la Specifica attendibile.
func Parse(r io.Reader) (*Specification, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()

	var s Specification
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("spec: parse failed: %w", err)
	}
	if dec.More() {
		return nil, ErrTrailingData
	}

	return &s, nil
}

// ParseAndValidate decodifica e valida in un solo passo una Specifica da r.
func ParseAndValidate(r io.Reader) (*Specification, error) {
	s, err := Parse(r)
	if err != nil {
		return nil, err
	}
	if err := Validate(s); err != nil {
		return nil, err
	}
	return s, nil
}
