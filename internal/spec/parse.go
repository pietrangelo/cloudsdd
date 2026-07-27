package spec

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ErrTrailingData indicates that the payload contains data beyond the first valid JSON value.
var ErrTrailingData = errors.New("spec: unexpected trailing data after JSON value")

// Parse decodes a Specification from r in strict mode: JSON fields not
// anticipated by the schema cause an error instead of being silently
// ignored or assigned (defense against Mass Assignment, RFC 001 §3).
//
// Parse performs no domain validation: call Validate (or use
// ParseAndValidate) before considering the Specification trustworthy.
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

// ParseAndValidate decodes and validates a Specification from r in a single step.
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
