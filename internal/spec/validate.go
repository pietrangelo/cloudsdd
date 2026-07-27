package spec

import (
	"fmt"
	"regexp"

	"github.com/go-playground/validator/v10"
)

// resourceIDPattern rispecchia il pattern definito nel JSON Schema
// (docs/rfc/001-core-architecture-and-json-schema.md, $defs.resource.id).
var resourceIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,63}$`)

var validate = newValidator()

func newValidator() *validator.Validate {
	v := validator.New(validator.WithRequiredStructEnabled())
	if err := v.RegisterValidation("resourceid", validateResourceID); err != nil {
		// Errore possibile solo per un nome di tag duplicato/non valido a
		// compile time: un bug nel codice, non una condizione runtime.
		panic(fmt.Sprintf("spec: impossibile registrare il validator resourceid: %v", err))
	}
	return v
}

func validateResourceID(fl validator.FieldLevel) bool {
	return resourceIDPattern.MatchString(fl.Field().String())
}

// Validate applica le regole di dominio alla Specifica già decodificata
// (secondo livello di validazione oltre alla decodifica strict di Parse,
// RFC 001 §3 "Validazione a doppio livello").
func Validate(s *Specification) error {
	if err := validate.Struct(s); err != nil {
		return fmt.Errorf("spec: validation failed: %w", err)
	}
	return nil
}
