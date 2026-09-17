// Copyright (c) 2025 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

//go:build js

package sqlite_wasm_js

import (
	"fmt"
)

type jsError struct {
	val any
}

func (e jsError) Unwrap() error {
	err, ok := e.val.(error)
	if ok {
		return err
	}
	return nil
}

func (e jsError) Error() string {
	if err := e.Unwrap(); err != nil {
		return err.Error()
	}
	return fmt.Sprintf("%v", e.val)
}

// catchIntoError converts a panic from syscall/js (a thrown JS exception)
// into an error. SQLite result codes never come through here: the JS side
// reports them in the result buffer so they become *Error values.
func catchIntoError(into *error) {
	if r := recover(); r != nil {
		*into = jsError{val: r}
	}
}
