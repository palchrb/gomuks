// gomuks - A Matrix client written in Go.
// Copyright (C) 2025 Tulir Asokan
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package hicli

import (
	"context"
	"errors"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
)

// isConnectionError reports whether err is a request that never got an
// answer: the device is offline, DNS failed, or the server is down. Anything
// the server did answer, and errors that aren't about a request at all, are
// real failures.
func isConnectionError(err error) bool {
	var httpErr mautrix.HTTPError
	return errors.As(err, &httpErr) && httpErr.Response == nil && httpErr.RespError == nil
}

// finishStartupChecks redoes the checks Start had to skip because the server
// was unreachable. It runs after the first successful sync, so the server is
// reachable now; if it isn't after all, the next sync tries again.
func (h *HiClient) finishStartupChecks(ctx context.Context) {
	log := zerolog.Ctx(ctx)
	err := h.CheckServerVersions(ctx)
	if isConnectionError(err) {
		h.startupChecksPending.Store(true)
		return
	} else if err != nil {
		// Start would have refused to run at all with this error.
		log.Err(err).Msg("Server version check failed after connecting, stopping sync")
		if fn := h.stopSync.Load(); fn != nil {
			(*fn)()
		}
		h.markSyncErrored(err, true)
		return
	}
	state, err := h.checkIsCurrentDeviceVerified(ctx, false)
	if isConnectionError(err) {
		h.startupChecksPending.Store(true)
		return
	} else if err != nil {
		log.Err(err).Msg("Failed to check device verification after connecting")
		return
	}
	state.StateChecked = true
	if state != h.VerificationState {
		log.Warn().
			Any("local_state", h.VerificationState).
			Any("checked_state", state).
			Msg("Verification state differs from what the local database said")
		h.VerificationState = state
		h.dispatchCurrentState()
	} else {
		log.Debug().Msg("Verification state confirmed after connecting")
	}
}
