// Copyright (c) 2025 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package sqlite_wasm_js

import (
	"fmt"
	"strings"
)

// Defaults for the OPFS SAHPool VFS. The pool has no shared memory, so
// journal_mode=WAL silently falls back to a rollback journal. In rollback
// mode with normal locking, every read transaction has to probe for a hot
// journal and re-read the change counter from OPFS, which makes point lookups
// an order of magnitude slower than in memory. EXCLUSIVE locking keeps the
// file lock across transactions (it must be set before journal_mode), and
// PERSIST keeps the journal file around instead of creating and deleting it
// on every transaction. Both require that only one connection uses the file.
const (
	defaultLockingMode = "EXCLUSIVE"
	defaultJournalMode = "PERSIST"
)

// SQLite journal modes, lowercase (the mode is upper-cased before use).
var validJournalModes = map[string]bool{
	"delete": true, "truncate": true, "persist": true, "memory": true, "wal": true, "off": true,
}

// parseLockingMode validates the _locking_mode connection parameter.
func parseLockingMode(val string) (string, error) {
	switch mode := strings.ToUpper(val); mode {
	case "":
		return defaultLockingMode, nil
	case "NORMAL", "EXCLUSIVE":
		return mode, nil
	default:
		return "", fmt.Errorf("invalid locking mode %q", val)
	}
}

// parseJournalMode validates the _journal_mode connection parameter.
func parseJournalMode(val string) (string, error) {
	if val == "" {
		return defaultJournalMode, nil
	}
	if !validJournalModes[strings.ToLower(val)] {
		return "", fmt.Errorf("invalid journal mode %q", val)
	}
	return strings.ToUpper(val), nil
}

// isDDL reports whether the query may change the schema, which would make
// the column metadata cached on prepared statements stale.
func isDDL(query string) bool {
	query = strings.TrimSpace(query)
	for _, prefix := range []string{"CREATE", "ALTER", "DROP"} {
		if len(query) >= len(prefix) && strings.EqualFold(query[:len(prefix)], prefix) {
			return true
		}
	}
	return false
}
