// Copyright (c) 2025 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package sqlite_wasm_js

import (
	"strings"
	"testing"
)

func TestParseLockingMode(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		err      bool
	}{
		{"", defaultLockingMode, false},
		{"normal", "NORMAL", false},
		{"Exclusive", "EXCLUSIVE", false},
		{"shared", "", true},
	} {
		got, err := parseLockingMode(tc.in)
		if (err != nil) != tc.err || got != tc.want {
			t.Errorf("parseLockingMode(%q) = %q, %v; want %q, err=%v", tc.in, got, err, tc.want, tc.err)
		}
	}
}

func TestParseJournalMode(t *testing.T) {
	if got, err := parseJournalMode(""); err != nil || got != defaultJournalMode {
		t.Errorf("parseJournalMode(\"\") = %q, %v; want default", got, err)
	}
	for _, in := range []string{"wal", "Persist", "delete", "truncate", "memory", "off"} {
		got, err := parseJournalMode(in)
		if err != nil || got != strings.ToUpper(in) {
			t.Errorf("parseJournalMode(%q) = %q, %v; want upper-cased input", in, got, err)
		}
	}
	if _, err := parseJournalMode("bogus"); err == nil {
		t.Error("parseJournalMode(\"bogus\") should fail")
	}
}

func TestIsDDL(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"CREATE TABLE x (a)", true},
		{"  create index i on x(a)", true},
		{"ALTER TABLE x ADD b", true},
		{"drop trigger t", true},
		{"SELECT * FROM x", false},
		{"INSERT INTO x VALUES (1)", false},
		{"PRAGMA journal_mode", false},
		{"", false},
		{"CREATED", true}, // prefix match is intentional: better to flush too often than too rarely
	} {
		if got := isDDL(tc.in); got != tc.want {
			t.Errorf("isDDL(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
