// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

// Package state maintains the Global Ledger (RFC 009): a local record of
// what CloudSDD has deployed, injected into the translation prompt so the
// AI can resolve cross-resource and cross-account references.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"cloudsdd/internal/config"
	"cloudsdd/internal/provider"
	"cloudsdd/internal/spec"
)

// Ledger is the aggregated set of resources CloudSDD has deployed.
type Ledger struct {
	LastUpdated time.Time       `json:"last_updated"`
	Resources   []spec.Resource `json:"resources"`
}

// mu serializes access within this process. It is necessary but not
// sufficient: see lockFile for the cross-process half (RFC 011 §2.7).
var mu sync.Mutex

// ledgerKey identifies a deployed resource uniquely across the scoping
// dimensions RFC 005 established.
//
// RFC 011 §1.1G1: the ledger previously matched on Resource.ID alone, so
// the same logical resource deployed to dev and to prod collapsed into a
// single entry, and destroying it in dev deleted the prod record too. That
// corrupts the very context RFC 009 exists to provide.
type ledgerKey struct {
	Account     string
	Environment string
	Region      string
	ID          string
}

func keyOf(r spec.Resource) ledgerKey {
	return ledgerKey{
		Account:     r.Account,
		Environment: r.Scope.Environment,
		Region:      r.Scope.Region,
		ID:          r.ID,
	}
}

// dir returns the directory the ledger lives in, which is the same
// owner-only CloudSDD home the configuration uses.
func dir() (string, error) {
	d, err := config.Dir()
	if err != nil {
		return "", fmt.Errorf("state: %w", err)
	}
	return d, nil
}

func ledgerPath() (string, error) {
	d, err := dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "ledger.json"), nil
}

func readLedger() (*Ledger, error) {
	p, err := ledgerPath()
	if err != nil {
		return nil, err
	}

	// #nosec G304 -- p is derived from the operator-controlled CloudSDD
	// home directory, never from Specification or model input.
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return &Ledger{Resources: []spec.Resource{}}, nil
		}
		return nil, fmt.Errorf("state: failed to read ledger %q: %w", p, err)
	}

	var l Ledger
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, fmt.Errorf("state: failed to parse ledger %q: %w", p, err)
	}
	// A ledger written before RFC 011 carries no scope fields; they
	// unmarshal to the empty string, which is exactly their previous
	// meaning, so old files keep working unchanged.
	if l.Resources == nil {
		l.Resources = []spec.Resource{}
	}
	return &l, nil
}

// writeLedger persists the ledger atomically: a temp file in the same
// directory, renamed over the target. An interrupted write can no longer
// truncate the ledger (RFC 011 §2.7).
func writeLedger(l *Ledger) error {
	p, err := ledgerPath()
	if err != nil {
		return err
	}

	// Never marshal as `null` (RFC 011 §1.1G4).
	if l.Resources == nil {
		l.Resources = []spec.Resource{}
	}

	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return fmt.Errorf("state: failed to marshal ledger: %w", err)
	}

	return writeTempAndRename(filepath.Dir(p), p, b)
}

// writeTempAndRename writes data to a temp file in dir and renames it over
// target. The close error is joined into the returned error rather than
// dropped: a failed Close can mean the data never reached the filesystem,
// so swallowing it would report a successful write that did not happen.
func writeTempAndRename(dir, target string, data []byte) (err error) {
	tmp, err := os.CreateTemp(dir, ".ledger-*.json")
	if err != nil {
		return fmt.Errorf("state: failed to create temp ledger: %w", err)
	}
	tmpName := tmp.Name()

	closed := false
	defer func() {
		if !closed {
			err = errors.Join(err, tmp.Close())
		}
		// No-op once the rename has succeeded.
		if removeErr := os.Remove(tmpName); removeErr != nil && !os.IsNotExist(removeErr) && err == nil {
			err = fmt.Errorf("state: failed to clean up temp ledger: %w", removeErr)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("state: failed to set ledger permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("state: failed to write ledger: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("state: failed to flush ledger: %w", err)
	}

	closed = true
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("state: failed to close temp ledger: %w", err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		return fmt.Errorf("state: failed to install ledger: %w", err)
	}
	return nil
}

// ReadLedger returns the current ledger.
func ReadLedger() (*Ledger, error) {
	mu.Lock()
	defer mu.Unlock()

	unlock, err := lockFile()
	if err != nil {
		return nil, err
	}
	defer unlock()

	return readLedger()
}

// RecordDeployment records the resources that were actually applied.
//
// It takes []provider.Result alongside the Specification's resources (RFC
// 011 §2.7) so a partially-failed Apply still records what succeeded:
// previously the CLI passed the whole Specification and returned early on
// error, so resources that had in fact been created went untracked (RFC
// 011 §1.1G2).
func RecordDeployment(resources []spec.Resource, results []provider.Result) error {
	applied := matchResults(resources, results, provider.StatusApplied)
	if len(applied) == 0 {
		return nil
	}

	mu.Lock()
	defer mu.Unlock()

	unlock, err := lockFile()
	if err != nil {
		return err
	}
	defer unlock()

	l, err := readLedger()
	if err != nil {
		return err
	}

	index := make(map[ledgerKey]int, len(l.Resources))
	for i, existing := range l.Resources {
		index[keyOf(existing)] = i
	}

	for _, r := range applied {
		if i, ok := index[keyOf(r)]; ok {
			l.Resources[i] = r
			continue
		}
		index[keyOf(r)] = len(l.Resources)
		l.Resources = append(l.Resources, r)
	}

	l.LastUpdated = time.Now().UTC()
	return writeLedger(l)
}

// RecordDestruction removes the resources that were actually destroyed.
func RecordDestruction(resources []spec.Resource, results []provider.Result) error {
	destroyed := matchResults(resources, results, provider.StatusDestroyed)
	if len(destroyed) == 0 {
		return nil
	}

	mu.Lock()
	defer mu.Unlock()

	unlock, err := lockFile()
	if err != nil {
		return err
	}
	defer unlock()

	l, err := readLedger()
	if err != nil {
		return err
	}

	remove := make(map[ledgerKey]struct{}, len(destroyed))
	for _, r := range destroyed {
		remove[keyOf(r)] = struct{}{}
	}

	// Always non-nil, so an emptied ledger marshals as [] not null.
	updated := make([]spec.Resource, 0, len(l.Resources))
	for _, existing := range l.Resources {
		if _, gone := remove[keyOf(existing)]; gone {
			continue
		}
		updated = append(updated, existing)
	}

	l.Resources = updated
	l.LastUpdated = time.Now().UTC()
	return writeLedger(l)
}

// matchResults pairs each Result carrying the wanted Status back to the
// Resource it came from, scoped to the Result's region so a multi-region
// resource yields one ledger entry per region actually acted on.
func matchResults(resources []spec.Resource, results []provider.Result, want provider.Status) []spec.Resource {
	byID := make(map[string]spec.Resource, len(resources))
	for _, r := range resources {
		byID[r.ID] = r
	}

	out := make([]spec.Resource, 0, len(results))
	for _, res := range results {
		if res.Status != want {
			continue
		}
		r, ok := byID[res.ResourceID]
		if !ok {
			continue
		}
		// Pin the entry to the region this Result was produced for, and
		// drop the plural form: the ledger records what exists, one row
		// per deployed instance, never the multi-region request shape.
		r.Scope.Region = res.Region
		r.Scope.Regions = nil
		out = append(out, r)
	}
	return out
}

// Lock tuning: how long we wait for another process to release the ledger,
// and how old a lock must be before it is treated as abandoned. Variables
// rather than constants so tests can exercise the contended path without
// sitting for the full timeout.
var (
	lockTimeout    = 10 * time.Second
	lockPollPeriod = 50 * time.Millisecond
	lockStaleAfter = 2 * time.Minute
)

// ErrLedgerLocked indicates another CloudSDD process holds the ledger.
var ErrLedgerLocked = errors.New("state: ledger is locked by another process")

// lockFile acquires a cross-process advisory lock on the ledger and
// returns the release function.
//
// RFC 009 §2.3 specified "file locking to prevent corruption", but the
// implementation only ever had a process-local sync.Mutex, so two
// concurrent cloudsdd invocations could still interleave read-modify-write
// and lose entries (RFC 011 §1.1G3). O_CREATE|O_EXCL is atomic on POSIX
// and on Windows, which keeps the dependency footprint at zero — relevant
// given the project's AGPLv3 dependency audit.
func lockFile() (func(), error) {
	d, err := dir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(d, "ledger.lock")

	deadline := time.Now().Add(lockTimeout)
	for {
		// #nosec G304 -- path is derived from the operator-controlled
		// CloudSDD home directory, never from Specification or model input.
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			// Record the holder so an abandoned lock is diagnosable. A
			// failure to write or close the marker does not invalidate
			// the lock itself — O_EXCL already granted it — but it must
			// not pass silently either.
			if _, writeErr := fmt.Fprintf(f, "%d\n", os.Getpid()); writeErr != nil {
				err = writeErr
			}
			if closeErr := f.Close(); closeErr != nil && err == nil {
				err = closeErr
			}
			if err != nil {
				if removeErr := os.Remove(path); removeErr != nil {
					err = errors.Join(err, removeErr)
				}
				return nil, fmt.Errorf("state: failed to write ledger lock: %w", err)
			}
			return func() { _ = os.Remove(path) }, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("state: failed to acquire ledger lock: %w", err)
		}

		// Reclaim a lock left behind by a process that died before
		// releasing it. A racing reclaim by another process is benign:
		// whichever loses simply retries the O_EXCL create.
		if info, statErr := os.Stat(path); statErr == nil {
			if time.Since(info.ModTime()) > lockStaleAfter {
				if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
					return nil, fmt.Errorf("state: failed to reclaim stale ledger lock: %w", removeErr)
				}
				continue
			}
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%w (%s)", ErrLedgerLocked, path)
		}
		time.Sleep(lockPollPeriod)
	}
}
