// Package auth implements API-key authentication for the kube-state-graph
// HTTP API.
//
// A KeySet holds the active set of accepted API keys. Keys are loaded from a
// file (one per line; `#` comments allowed) and/or a comma-separated env
// value. Validation uses constant-time comparison and iterates the full
// stored set on every call so neither match latency nor early exit leaks key
// count or position.
//
// File-backed key sets support hot reload: LoadFile performs the initial
// (startup) load, where an empty file is the documented auth-disabled dev
// default; ReloadFile is for subsequent re-reads and refuses to replace a
// non-empty active set with an empty one, so a truncated or comment-only file
// observed mid-rotation cannot silently disable authentication. The package
// provides no ticker of its own — cmd/kube-state-graph runs a background loop
// that calls ReloadFile periodically, and each successful call atomically
// swaps the active set so a Kubernetes Secret rotation is picked up without a
// process restart.
package auth

import (
	"bufio"
	"crypto/subtle"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
)

// KeySet is the live set of accepted API keys. Safe for concurrent use.
type KeySet struct {
	keys atomic.Pointer[[]string]
}

// NewKeySet returns an empty KeySet (auth-disabled).
func NewKeySet() *KeySet {
	ks := &KeySet{}
	empty := []string{}
	ks.keys.Store(&empty)
	return ks
}

// LoadFile reads keys from path (one per line, blanks + `#` comments
// stripped) and atomically replaces the active set. Intended for the initial
// startup load only: an empty file yields an empty set (the documented
// auth-disabled dev default). For periodic re-reads use ReloadFile, which
// fails closed instead.
func (ks *KeySet) LoadFile(path string) error {
	keys, err := readKeyFile(path)
	if err != nil {
		return err
	}
	ks.keys.Store(&keys)
	return nil
}

// ReloadFile re-reads keys from path and atomically replaces the active set,
// failing closed on an empty result: when the file parses to ZERO keys while
// the currently active set is non-empty, the active set is kept and an error
// is returned — an empty/comment-only/truncated file observed mid-rotation
// must not silently disable authentication on a live server. Replacing an
// empty set with an empty set is not an error.
//
// The returned changed flag reports whether the new SET of keys differs from
// the previous one (order-insensitive — a reorder-only rewrite is not a
// change). Count comparison is not enough: the common rotation shape swaps N
// old keys for N new ones, and the caller's "keys reloaded" log must fire for
// it. Key values never leave this package.
func (ks *KeySet) ReloadFile(path string) (changed bool, err error) {
	keys, err := readKeyFile(path)
	if err != nil {
		return false, err
	}
	old := ks.keys.Load()
	if len(keys) == 0 && old != nil && len(*old) > 0 {
		return false, fmt.Errorf("refusing to replace %d active keys with an empty key set from %s", len(*old), path)
	}
	ks.keys.Store(&keys)
	return !sameKeySet(old, keys), nil
}

// sameKeySet reports whether old (nillable snapshot) and next hold the same
// keys as sets. Both slices are already deduplicated by their loaders, so a
// length check plus one-way membership suffices. Not constant-time — both
// operands are server-held, never attacker input.
func sameKeySet(old *[]string, next []string) bool {
	var prev []string
	if old != nil {
		prev = *old
	}
	if len(prev) != len(next) {
		return false
	}
	seen := make(map[string]struct{}, len(prev))
	for _, k := range prev {
		seen[k] = struct{}{}
	}
	for _, k := range next {
		if _, ok := seen[k]; !ok {
			return false
		}
	}
	return true
}

// LoadCSV parses comma-separated keys (whitespace trimmed; blanks dropped)
// and atomically replaces the active set.
func (ks *KeySet) LoadCSV(csv string) {
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	seen := map[string]struct{}{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	ks.keys.Store(&out)
}

// Empty reports whether the set holds zero keys (auth disabled).
func (ks *KeySet) Empty() bool {
	keys := ks.keys.Load()
	return keys == nil || len(*keys) == 0
}

// Validate reports whether presented matches any stored key. Comparison is
// constant-time per stored key and always iterates the full set, so neither
// match latency nor early exit leaks the matching position or the key count.
func (ks *KeySet) Validate(presented string) bool {
	if presented == "" {
		return false
	}
	keys := ks.keys.Load()
	if keys == nil || len(*keys) == 0 {
		return false
	}
	pb := []byte(presented)
	matched := 0
	for _, k := range *keys {
		kb := []byte(k)
		if len(kb) != len(pb) {
			// A different length cannot match; skip without a self-compare (the
			// previous subtle.ConstantTimeCompare(kb, kb) was a no-op against
			// itself). The whole set is still iterated — no early return — so key
			// count / position is not leaked via timing. API keys are
			// high-entropy tokens, so a per-key length difference is not a useful
			// timing oracle.
			continue
		}
		if subtle.ConstantTimeCompare(kb, pb) == 1 {
			matched = 1
		}
	}
	return matched == 1
}

// Snapshot returns the current key count for diagnostics. The keys themselves
// are never exposed.
func (ks *KeySet) Snapshot() int {
	keys := ks.keys.Load()
	if keys == nil {
		return 0
	}
	return len(*keys)
}

func readKeyFile(path string) ([]string, error) {
	if path == "" {
		return nil, errors.New("api-keys-file path is empty")
	}
	f, err := os.Open(path) //nolint:gosec // path is operator-supplied config
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	out := make([]string, 0, 8)
	seen := map[string]struct{}{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if _, dup := seen[line]; dup {
			continue
		}
		seen[line] = struct{}{}
		out = append(out, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return out, nil
}
