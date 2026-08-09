// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package spec

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/go-playground/validator/v10"

	"cloudsdd/internal/schedule"
)

// resourceIDPattern mirrors the pattern defined in the JSON Schema
// (docs/rfc/001-core-architecture-and-json-schema.md, $defs.resource.id).
var resourceIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,63}$`)

// scopeNamePattern constrains Scope.Environment (RFC 005 §2.2): same
// charset as resourceIDPattern, shorter max length appropriate for a
// label rather than an identifier.
var scopeNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,32}$`)

// mountPathSegment constrains one directory component of a Volume.MountPath
// (RFC 018 §2.7). Requiring every segment to start with a letter or a digit
// is what disqualifies "." and "..", and the closed charset is what
// disqualifies whitespace and control bytes: a mount point is a directory a
// human names, not an arbitrary byte string handed to a container runtime.
var mountPathSegment = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// maxMountPathLen caps a whole mount path. POSIX would allow sixteen times
// this; the cap is not about what a kernel accepts but about what a person
// writes, since the path is echoed in plans and derived into provider
// resource names.
const maxMountPathLen = 255

var validate = newValidator()

func newValidator() *validator.Validate {
	v := validator.New(validator.WithRequiredStructEnabled())
	if err := v.RegisterValidation("resourceid", validateResourceID); err != nil {
		// This error can only happen for a duplicate/invalid tag name at
		// compile time: a bug in the code, not a runtime condition.
		panic(fmt.Sprintf("spec: failed to register the resourceid validator: %v", err))
	}
	if err := v.RegisterValidation("scopename", validateScopeName); err != nil {
		panic(fmt.Sprintf("spec: failed to register the scopename validator: %v", err))
	}
	if err := v.RegisterValidation("mountpath", stringValidator(validMountPath)); err != nil {
		panic(fmt.Sprintf("spec: failed to register the mountpath validator: %v", err))
	}
	// The schedule tags (RFC 012 §2.1) delegate to internal/schedule
	// rather than restating its formats here: the package that compiles a
	// time is the package that decides what a valid time looks like.
	for tag, fn := range map[string]func(string) bool{
		"clocktime":    schedule.ValidClockTime,
		"ianatz":       schedule.ValidTimezone,
		"scheduledate": schedule.ValidDate,
	} {
		if err := v.RegisterValidation(tag, stringValidator(fn)); err != nil {
			panic(fmt.Sprintf("spec: failed to register the %s validator: %v", tag, err))
		}
	}
	return v
}

// stringValidator adapts a plain string predicate to validator.Func.
func stringValidator(fn func(string) bool) validator.Func {
	return func(fl validator.FieldLevel) bool { return fn(fl.Field().String()) }
}

func validateResourceID(fl validator.FieldLevel) bool {
	return resourceIDPattern.MatchString(fl.Field().String())
}

func validateScopeName(fl validator.FieldLevel) bool {
	return scopeNamePattern.MatchString(fl.Field().String())
}

// validMountPath reports whether path is an absolute POSIX path naming a
// directory below the root (RFC 018 §2.7).
//
// Splitting on "/" and demanding a well-formed segment each time is what
// makes the refusals uniform: "/" is a single empty segment, a trailing
// slash is a final empty one, "//" is an interior one, and "/var/../etc" is
// a segment that starts with a dot. No path is cleaned or repaired here — a
// mount path CloudSDD rewrote would mount somewhere the document does not
// say.
func validMountPath(path string) bool {
	if len(path) > maxMountPathLen || !strings.HasPrefix(path, "/") {
		return false
	}
	for _, segment := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if !mountPathSegment.MatchString(segment) {
			return false
		}
	}
	return true
}

// Validate applies domain rules to the already-decoded Specification
// (second tier of validation on top of Parse's strict decoding,
// RFC 001 §3 "Two-tier validation").
func Validate(s *Specification) error {
	if err := validate.Struct(s); err != nil {
		return fmt.Errorf("spec: validation failed: %w", err)
	}
	for _, r := range s.Resources {
		if err := validateVolumes(r); err != nil {
			return fmt.Errorf("spec: resource %q: %w", r.ID, err)
		}
	}
	return nil
}

// validateVolumes applies the two filesystem rules a struct tag cannot
// state, because both are about a volume's context rather than its shape
// (RFC 018 §2.7).
//
// The first is the pair (Type, Volumes): every resource type can spell the
// field, only one has anywhere to mount it. The second is the pair of
// volumes: within one resource a name and a mount path each identify one
// filesystem, so a repeat is a Specification that cannot be honoured rather
// than one that is merely odd — the second entry would have to overwrite
// the first.
func validateVolumes(r Resource) error {
	if len(r.Volumes) == 0 {
		return nil
	}
	if !r.Type.MountsFilesystem() {
		return fmt.Errorf("a %s cannot mount a filesystem", r.Type)
	}

	names := make(map[string]bool, len(r.Volumes))
	paths := make(map[string]bool, len(r.Volumes))
	for _, v := range r.Volumes {
		if names[v.Name] {
			return fmt.Errorf("volume %q is declared twice", v.Name)
		}
		if paths[v.MountPath] {
			return fmt.Errorf("volume %q reuses the mount path %q", v.Name, v.MountPath)
		}
		names[v.Name], paths[v.MountPath] = true, true
	}
	return nil
}
