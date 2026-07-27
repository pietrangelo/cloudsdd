package spec

import (
	"strings"
	"testing"
)

// FuzzParse exercises Parse against malformed or hostile JSON input
// (RFC 001 §3 "Fuzzing"). The goal is not for Parse to accept the input,
// but for it to never panic and to always return an explicit error for
// invalid input.
func FuzzParse(f *testing.F) {
	seeds := []string{
		`{"sdd_version": "1.0", "intent": "deploy", "resources": [{"id": "a", "type": "relational_database", "provider": "agnostic", "properties": {}}]}`,
		`{}`,
		`null`,
		`[]`,
		`{"sdd_version": "1.0"`,
		`{"resources": {"id": "a"}}`,
		`{"sdd_version": 1.0, "intent": "deploy", "resources": []}`,
		``,
		`{"__proto__": {"polluted": true}}`,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, input string) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Parse panicked on input %q: %v", input, r)
			}
		}()
		_, _ = Parse(strings.NewReader(input))
	})
}
