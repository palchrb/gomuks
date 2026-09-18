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

//go:build js

package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"fmt"
	"image"
	"io"
	"mime"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall/js"
	"time"

	"github.com/buckket/go-blurhash"
	"github.com/rs/zerolog"
	"go.mau.fi/util/jsontime"
	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/gomuks/pkg/gomuks"
	"go.mau.fi/gomuks/pkg/hicli/database"
	"go.mau.fi/gomuks/pkg/hicli/jsoncmd"
)

// uploadExtras carries what the server build works out with ffmpeg and we let
// the browser work out instead: the duration and dimensions of audio and
// video, a frame to use as a thumbnail, and the waveform of a voice message.
// See probeMedia in web/src/api/wasm/probe.ts.
type uploadExtras struct {
	DurationMS int   `json:"duration_ms,omitempty"`
	Width      int   `json:"width,omitempty"`
	Height     int   `json:"height,omitempty"`
	Waveform   []int `json:"waveform,omitempty"`
}

func uploadMedia(
	ctx context.Context,
	params jsoncmd.UploadMediaParams,
	extras uploadExtras,
	payload, thumbnail []byte,
) (*event.MessageEventContent, error) {
	log := zerolog.Ctx(ctx)
	// Same image handling as the server build, which is pure Go. The video
	// and audio targets need ffmpeg and there is none here, so they are
	// reported rather than silently ignored.
	if encoded, err := gomuks.ReencodeImage(bytes.NewReader(payload), params); err != nil {
		return nil, fmt.Errorf("failed to reencode media: %w", err)
	} else if encoded != nil {
		log.Debug().
			Str("encode_to", params.EncodeTo).
			Int("before", len(payload)).
			Int("after", len(encoded)).
			Msg("Re-encoded upload")
		payload = encoded
	} else if params.EncodeTo != "" {
		return nil, fmt.Errorf("re-encoding to %s needs ffmpeg, which the browser build doesn't have", params.EncodeTo)
	}
	msgType, info, defaultFileName, err := gmx.GenerateFileInfo(ctx, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("failed to generate file info: %w", err)
	}
	info.Size = len(payload)
	// GenerateFileInfo reads dimensions for images, but audio and video need
	// a demuxer, which is ffmpeg in the server build and the browser here.
	info.Duration = cmp.Or(info.Duration, extras.DurationMS)
	info.Width = cmp.Or(info.Width, extras.Width)
	info.Height = cmp.Or(info.Height, extras.Height)
	fileName := cmp.Or(params.Filename, defaultFileName)
	content := &event.MessageEventContent{
		MsgType:  msgType,
		Body:     fileName,
		Info:     info,
		FileName: fileName,
	}
	if params.ForceFile {
		content.MsgType = event.MsgFile
	} else if params.VoiceMessage {
		// The server build generates the waveform with ffmpeg; the browser
		// decodes the audio and sends the peaks instead. Without them the
		// message is still marked as a voice message.
		content.MSC1767Audio = &event.MSC1767Audio{
			Duration: info.Duration,
			Waveform: extras.Waveform,
		}
		content.MSC3245Voice = &event.MSC3245Voice{}
	}
	if len(thumbnail) > 0 {
		if err := attachThumbnail(ctx, info, thumbnail, params.Encrypt); err != nil {
			// A missing thumbnail is not worth failing the upload over, which
			// is also how the server build treats it.
			log.Warn().Err(err).Msg("Failed to attach thumbnail")
		}
	}
	checksum := sha256.Sum256(payload)
	content.File, content.URL, err = gmx.UploadFileDirect(
		ctx,
		checksum[:],
		bytes.NewReader(payload),
		params.Encrypt,
		int64(info.Size),
		info.MimeType,
		fileName,
		nil,
	)
	return content, err
}

// attachThumbnail uploads a thumbnail the browser produced and fills in the
// same fields generateVideoThumbnail does in the server build, including the
// blurhash, which is pure Go.
func attachThumbnail(ctx context.Context, info *event.FileInfo, thumbnail []byte, encrypt bool) error {
	thumbnailInfo := &event.FileInfo{
		MimeType: "image/jpeg",
		Size:     len(thumbnail),
	}
	if img, _, err := image.Decode(bytes.NewReader(thumbnail)); err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to decode thumbnail")
	} else {
		bounds := img.Bounds()
		thumbnailInfo.Width = bounds.Dx()
		thumbnailInfo.Height = bounds.Dy()
		if hash, err := blurhash.Encode(4, 3, img); err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to generate thumbnail blurhash")
		} else {
			thumbnailInfo.AnoaBlurhash = hash
		}
	}
	checksum := sha256.Sum256(thumbnail)
	var err error
	info.ThumbnailFile, info.ThumbnailURL, err = gmx.UploadFileDirect(
		ctx, checksum[:], bytes.NewReader(thumbnail), encrypt,
		int64(len(thumbnail)), "image/jpeg", "thumbnail.jpeg", nil,
	)
	if err != nil {
		return err
	}
	info.ThumbnailInfo = thumbnailInfo
	return nil
}

func realJSDownloadCallback(ctx context.Context, path, rawQuery string, callbacks js.Value) {
	resolved := false
	defer func() {
		if !resolved {
			callbacks.Call("reject")
		}
	}()
	log := zerolog.Ctx(ctx)
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		log.Error().Msg("Invalid media download path")
		return
	}
	mxc := id.ContentURI{
		Homeserver: parts[len(parts)-2],
		FileID:     parts[len(parts)-1],
	}
	query, err := url.ParseQuery(strings.TrimPrefix(rawQuery, "?"))
	if err != nil {
		log.Err(err).Msg("Failed to parse media download query")
		return
	}
	encrypted, _ := strconv.ParseBool(query.Get("encrypted"))
	useThumbnail := query.Get("thumbnail") == "avatar"
	fallback := query.Get("fallback")
	// Set when the fallback is served because a download failed, to the time
	// the next attempt is allowed. The server build does the same thing with a
	// Cache-Control max-age on the fallback response, so the browser shows the
	// letter avatar for exactly as long as the backoff lasts and then asks
	// again. The Cache API has no expiry of its own, so the time is carried in
	// a header and checked by the service worker.
	var retryAfter float64
	if fallback != "" {
		fallbackParts := strings.Split(fallback, ":")
		defer func() {
			if !resolved {
				log.Debug().Msg("Returning fallback avatar")
				data := gomuks.MakeFallbackAvatar(fallbackParts[0], fallbackParts[1])
				buf := js.Global().Get("Uint8Array").New(len(data))
				js.CopyBytesToJS(buf, data)
				resp := map[string]any{
					"buffer":             buf,
					"contentType":        "image/svg+xml",
					"contentDisposition": "",
					"csp":                database.MediaContentSecurityPolicy,
				}
				if retryAfter > 0 {
					resp["retryAfter"] = retryAfter
				}
				callbacks.Call("resolve", js.ValueOf(resp))
				resolved = true
			}
		}()
	}
	cacheEntry, err := gmx.Client.DB.Media.Get(ctx, mxc)
	if err != nil {
		log.Err(err).Msg("Failed to get cached media entry")
		return
	} else if (cacheEntry == nil || cacheEntry.EncFile == nil) && encrypted {
		log.Error().Msg("Tried to download encrypted media without keys")
		return
	} else if cacheEntry != nil && cacheEntry.EncFile != nil && !encrypted {
		log.Error().Msg("Tried to download encrypted media without encrypted flag")
		return
	}
	// A download that failed is remembered with a growing backoff, the same as
	// the server build does, so a file the homeserver no longer has isn't
	// re-requested on every render. The deferred fallback above answers with
	// the letter avatar in the meantime.
	if cacheEntry != nil && cacheEntry.Error.UseCache() {
		log.Debug().
			Int("attempts", cacheEntry.Error.Attempts).
			Time("last_attempt", cacheEntry.Error.ReceivedAt.Time).
			Msg("Not retrying media download yet")
		retryAfter = cappedRetryAfter(cacheEntry.Error.NextRetry())
		return
	}
	resp, err := gmx.Client.Client.Download(mautrix.WithMaxRetries(ctx, 0), mxc)
	if err != nil {
		log.Err(err).Msg("Failed to download media")
		retryAfter = cappedRetryAfter(rememberMediaError(ctx, mxc, cacheEntry))
		return
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Err(err).Msg("Failed to read media data")
		retryAfter = cappedRetryAfter(rememberMediaError(ctx, mxc, cacheEntry))
		return
	}
	if cacheEntry != nil && cacheEntry.EncFile != nil {
		err = cacheEntry.EncFile.DecryptInPlace(data)
		if err != nil {
			// The server build records these too, so an undecryptable file
			// isn't fetched again every time it scrolls into view.
			log.Err(err).Msg("Failed to decrypt media data")
			retryAfter = cappedRetryAfter(rememberMediaError(ctx, mxc, cacheEntry))
			return
		}
	}
	// What the server build stores after a successful download, including
	// clearing the error. Without that the attempt count only ever grows, so
	// one old failure pushes the backoff towards its week-long cap and the
	// fallback avatar sticks around long after the file became available.
	if cacheEntry == nil {
		cacheEntry = &database.Media{MXC: mxc}
	}
	if cacheEntry.FileName == "" {
		_, respDisposition, _ := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
		cacheEntry.FileName = respDisposition["filename"]
	}
	if cacheEntry.MimeType == "" {
		cacheEntry.MimeType = resp.Header.Get("Content-Type")
	}
	cacheEntry.Size = int64(len(data))
	cacheEntry.Hash = ptr.Ptr(sha256.Sum256(data))
	cacheEntry.Error = nil
	if err = gmx.Client.DB.Media.Put(ctx, cacheEntry); err != nil {
		log.Err(err).Msg("Failed to save media cache entry")
	}
	contentType := cmp.Or(cacheEntry.MimeType, resp.Header.Get("Content-Type"))
	// The disposition is decided here rather than taken from the homeserver,
	// the same as the server build does in Media.ToHeaders: anything that
	// isn't on the safe list is a download rather than something the browser
	// renders in place.
	contentDisposition := mime.FormatMediaType(
		cacheEntry.ContentDisposition(),
		map[string]string{"filename": cacheEntry.FileName},
	)
	if useThumbnail && strings.HasPrefix(contentType, "image/") {
		thumbnail, thumbnailType, err := gomuks.MakeAvatarThumbnail(data, cmp.Or(gmx.Config.Media.ThumbnailSize, 120))
		if err != nil {
			log.Warn().Err(err).Msg("Failed to generate avatar thumbnail, serving full image")
		} else {
			data = thumbnail
			contentType = thumbnailType
			contentDisposition = ""
		}
	}
	buf := js.Global().Get("Uint8Array").New(len(data))
	js.CopyBytesToJS(buf, data)
	callbacks.Call("resolve", js.ValueOf(map[string]any{
		"buffer":             buf,
		"contentType":        contentType,
		"contentDisposition": contentDisposition,
		"csp":                database.MediaContentSecurityPolicy,
	}))
	resolved = true
	log.Debug().
		Str("content_type", contentType).
		Str("content_disposition", contentDisposition).
		Int("length", len(data)).
		Msg("Download successful")
}

// rememberMediaError records a failed download on the media cache entry with
// an incremented attempt count, which is what database.MediaError's backoff is
// calculated from. Mirrors addErrorToCacheEntry in pkg/gomuks/mediadownload.go,
// minus the HTTP specifics that only the server build needs.
// maxFallbackCache limits how long the browser may keep a fallback avatar.
// The real backoff can reach a week, and the Cache API has no way to notice
// that the situation changed, so a long-lived entry would outlive the reason
// for it. The backend enforces the real schedule on each ask, which costs
// nothing when it is still backing off.
const maxFallbackCache = 5 * time.Minute

func cappedRetryAfter(next time.Time) float64 {
	limit := time.Now().Add(maxFallbackCache)
	if next.After(limit) {
		next = limit
	}
	return float64(next.UnixMilli())
}

func rememberMediaError(ctx context.Context, mxc id.ContentURI, cacheEntry *database.Media) time.Time {
	log := zerolog.Ctx(ctx)
	if cacheEntry == nil {
		cacheEntry = &database.Media{MXC: mxc}
	}
	if cacheEntry.Error == nil {
		cacheEntry.Error = &database.MediaError{
			ReceivedAt: jsontime.UnixMilliNow(),
			Attempts:   1,
		}
	} else {
		cacheEntry.Error.Attempts++
		cacheEntry.Error.ReceivedAt = jsontime.UnixMilliNow()
	}
	if cacheEntry.Error.Matrix == nil {
		cacheEntry.Error.Matrix = ptr.Ptr(mautrix.MUnknown.WithMessage("Failed to download media"))
		cacheEntry.Error.StatusCode = http.StatusBadGateway
	}
	if err := gmx.Client.DB.Media.Put(ctx, cacheEntry); err != nil {
		log.Err(err).Msg("Failed to save errored media cache entry")
	}
	return cacheEntry.Error.NextRetry()
}

func jsDownloadCallback(_ js.Value, args []js.Value) any {
	path := args[0].String()
	query := args[1].String()
	callbacks := args[2]
	ctx := gmx.Log.With().
		Str("action", "wasmuks download").
		Str("path", path).
		Str("query", query).
		Logger().
		WithContext(context.Background())
	go func() {
		// A panic here would otherwise exit the Go runtime and take the whole
		// client with it. The callback rejects, which shows the fallback
		// avatar or a broken image.
		defer func() {
			if err := recover(); err != nil {
				zerolog.Ctx(ctx).Error().
					Bytes(zerolog.ErrorStackFieldName, debug.Stack()).
					Any(zerolog.ErrorFieldName, err).
					Msg("Panic while downloading media")
				callbacks.Call("reject")
			}
		}()
		realJSDownloadCallback(ctx, path, query, callbacks)
	}()
	return nil
}
