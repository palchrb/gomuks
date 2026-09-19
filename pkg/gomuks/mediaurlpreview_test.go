// gomuks - A Matrix client written in Go.
// Copyright (C) 2026 Tulir Asokan
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
package gomuks

import (
	"context"
	"io"
	"strings"
	"testing"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/gomuks/pkg/hicli/jsoncmd"
)

// The wasm build has no filesystem, so the upload of a URL preview image has
// to be replaceable: UploadMedia starts by writing a temp file.
func TestUploadPreviewImageUsesOverride(t *testing.T) {
	var gotBody string
	var gotParams jsoncmd.UploadMediaParams
	gmx := &Gomuks{UploadMediaFunc: func(
		_ context.Context, reader io.Reader, params jsoncmd.UploadMediaParams,
	) (*event.MessageEventContent, error) {
		body, err := io.ReadAll(reader)
		if err != nil {
			return nil, err
		}
		gotBody = string(body)
		gotParams = params
		return &event.MessageEventContent{URL: id.ContentURIString("mxc://example.org/uploaded")}, nil
	}}
	content, err := gmx.uploadPreviewImage(
		context.Background(), strings.NewReader("image bytes"), jsoncmd.UploadMediaParams{Encrypt: true})
	if err != nil {
		t.Fatalf("uploadPreviewImage: %v", err)
	}
	if gotBody != "image bytes" {
		t.Errorf("override got body %q", gotBody)
	}
	if !gotParams.Encrypt {
		t.Error("override didn't get the encrypt parameter")
	}
	if content.URL != "mxc://example.org/uploaded" {
		t.Errorf("content URL is %q", content.URL)
	}
}
