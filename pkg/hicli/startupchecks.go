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

// checkUnverifiedWithServer is the startup path for a device whose local
// database doesn't make it verified: the verification screen is next, and
// what it offers depends on the server (whether SSSS is set up), so this
// waits for the server. If the server can't be reached, the local state is
// used and the checks are redone once a sync goes through, which happens
// after verification.
func (h *HiClient) checkUnverifiedWithServer(ctx context.Context) error {
	err := h.CheckServerVersions(ctx)
	if isConnectionError(err) {
		// The server passed this check when the account logged in, so
		// not being able to reach it now is no reason to refuse to start.
		zerolog.Ctx(ctx).Warn().Err(err).
			Msg("Couldn't reach the server to check its versions, retrying after the first sync")
		h.startupChecksPending.Store(true)
	} else if err != nil {
		return err
	}
	h.VerificationState, err = h.checkIsCurrentDeviceVerified(ctx, false)
	if isConnectionError(err) {
		zerolog.Ctx(ctx).Warn().Err(err).
			Msg("Couldn't reach the server to check the key backup, using local verification state")
		h.VerificationState, err = h.checkIsCurrentDeviceVerified(ctx, true)
		h.VerificationState.HasSSSS = h.VerificationState.IsVerified
		h.startupChecksPending.Store(true)
	}
	if err != nil {
		return err
	}
	h.VerificationState.StateChecked = true
	return nil
}

// finishStartupChecks does the checks against the server that Start didn't
// wait for: the spec versions, and the key backup comparison that completes
// the verification state. It runs alongside the first sync of a verified
// device, and again after the first successful sync when the server
// couldn't be reached. An outdated server stops the sync, as Start would
// have refused to run; a verification state that differs from the local
// one is dispatched, and the frontend shows the verification screen as it
// would have at startup.
func (h *HiClient) finishStartupChecks(ctx context.Context) {
	log := zerolog.Ctx(ctx)
	err := h.CheckServerVersions(ctx)
	if isConnectionError(err) {
		h.startupChecksPending.Store(true)
		return
	} else if err != nil {
		log.Err(err).Msg("Server version check failed, stopping sync")
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
		log.Err(err).Msg("Failed to check device verification with the server")
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
		log.Debug().Msg("Verification state confirmed with the server")
	}
}
