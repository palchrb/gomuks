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

package main

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// A /sync long poll asks the server to wait up to 30 seconds, so a healthy
// request is answered within that plus a little latency. One that has been in
// flight longer is either a zombie or a very slow catch-up, and only the
// former should be restarted.
const healthySyncRequest = 35 * time.Second

// syncRequestTracker records when the current /sync request was sent, so that
// a resume can tell a request that was in flight across a suspension from one
// that is merely slow. It wraps the default transport, which on wasm is the
// browser's fetch.
type syncRequestTracker struct {
	next  http.RoundTripper
	lock  sync.Mutex
	start time.Time // zero when no /sync request is in flight
}

func (t *syncRequestTracker) RoundTrip(req *http.Request) (*http.Response, error) {
	isSync := strings.HasSuffix(req.URL.Path, "/sync")
	if isSync {
		t.lock.Lock()
		t.start = time.Now()
		t.lock.Unlock()
	}
	resp, err := t.next.RoundTrip(req)
	if isSync {
		t.lock.Lock()
		t.start = time.Time{}
		t.lock.Unlock()
	}
	return resp, err
}

func (t *syncRequestTracker) inFlightSince() time.Time {
	t.lock.Lock()
	defer t.lock.Unlock()
	return t.start
}

// trackSyncRequests installs the tracker on the client's HTTP transport, and
// reports whether it was already there. The server build clears the transport
// for wasm when it creates the client (see pkg/gomuks), which happens again
// after a logout, so this is called lazily rather than once.
func trackSyncRequests() (*syncRequestTracker, bool) {
	httpClient := gmx.Client.Client.Client
	if tracker, ok := httpClient.Transport.(*syncRequestTracker); ok {
		return tracker, true
	}
	tracker := &syncRequestTracker{next: http.DefaultTransport}
	httpClient.Transport = tracker
	return tracker, false
}

// onResume is what the frontend asks for when the tab becomes visible after
// being hidden since hiddenAt. iOS suspends a backgrounded PWA, and the /sync
// request that was in flight often comes back as a zombie: the fetch neither
// fails nor completes until the HTTP client's own timeout, minutes later,
// while the status still says ok. Restarting the sync loop aborts it and
// starts over from the same since token.
//
// But not every request in flight on resume is a zombie. A background tab on
// a desktop keeps syncing, so its current request was sent after the tab was
// hidden and is fine; and a large catch-up can legitimately take longer than
// a long poll, and restarting it would only make the server compute it again.
// So a restart needs all three: a request in flight, sent before the tab was
// hidden, and older than any healthy long poll can be.
func onResume(hiddenAt time.Time) {
	if gmx.Client == nil || !gmx.Client.IsSyncing() {
		return
	}
	tracker, known := trackSyncRequests()
	log := gmx.Log.With().Str("action", "resume").Logger()
	if !known {
		// Nothing is known about the request in flight, if any, so fall back
		// to restarting; from now on the tracker knows.
		log.Info().Msg("Tab resumed, restarting sync (request state unknown)")
		go gmx.Client.Sync()
		return
	}
	start := tracker.inFlightSince()
	switch {
	case start.IsZero():
		log.Debug().Msg("Tab resumed, no sync request in flight")
	case start.After(hiddenAt):
		log.Debug().Msg("Tab resumed, sync request was sent while hidden, leaving it")
	case time.Since(start) < healthySyncRequest:
		log.Debug().Dur("in_flight", time.Since(start)).Msg("Tab resumed, sync request is recent, leaving it")
	default:
		log.Info().Dur("in_flight", time.Since(start)).Msg("Tab resumed, sync request predates hiding and has overrun, restarting sync")
		go gmx.Client.Sync()
	}
}
