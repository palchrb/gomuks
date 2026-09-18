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

// The server build asks ffmpeg for the duration and dimensions of audio and
// video, for a frame to use as a thumbnail, and for the waveform of a voice
// message. A browser can do all of that itself, so the wasm build works it out
// here and sends the results along with the upload. Only re-encoding actually
// needs a codec we don't have.

export interface MediaProbe {
	duration_ms?: number
	width?: number
	height?: number
	waveform?: number[]
	thumbnail?: Uint8Array<ArrayBuffer>
}

// The frame to grab. Grabbing the very first one often gets a black frame, so
// take one a moment in, or the middle of a very short clip.
const THUMBNAIL_SECONDS = 1
const THUMBNAIL_QUALITY = 0.8
// A thumbnail this size is plenty for a timeline and keeps the upload small.
const THUMBNAIL_MAX_EDGE = 800
// What the server build asks ffmpeg for: one value per bucket, 0 to 256.
const WAVEFORM_MAX = 256
const WAVEFORM_MIN_BUCKETS = 30
const WAVEFORM_MAX_BUCKETS = 120

function waitForEvent(target: EventTarget, event: string, timeoutMS = 10_000): Promise<void> {
	return new Promise((resolve, reject) => {
		const timeout = setTimeout(() => {
			cleanup()
			reject(new Error(`timed out waiting for ${event}`))
		}, timeoutMS)
		const onDone = () => {
			cleanup()
			resolve()
		}
		const onError = () => {
			cleanup()
			reject(new Error(`error while waiting for ${event}`))
		}
		const cleanup = () => {
			clearTimeout(timeout)
			target.removeEventListener(event, onDone)
			target.removeEventListener("error", onError)
		}
		target.addEventListener(event, onDone, { once: true })
		target.addEventListener("error", onError, { once: true })
	})
}

async function probeVideo(file: Blob): Promise<MediaProbe> {
	const url = URL.createObjectURL(file)
	const video = document.createElement("video")
	video.preload = "metadata"
	video.muted = true
	try {
		video.src = url
		await waitForEvent(video, "loadedmetadata")
		const probe: MediaProbe = {
			width: video.videoWidth || undefined,
			height: video.videoHeight || undefined,
		}
		if (Number.isFinite(video.duration) && video.duration > 0) {
			probe.duration_ms = Math.round(video.duration * 1000)
		}
		if (!video.videoWidth || !video.videoHeight) {
			// Audio in a video container, so there is no frame to grab.
			return probe
		}
		video.currentTime = Math.min(THUMBNAIL_SECONDS, (video.duration || 0) / 2)
		await waitForEvent(video, "seeked")
		const scale = Math.min(1, THUMBNAIL_MAX_EDGE / Math.max(video.videoWidth, video.videoHeight))
		const canvas = document.createElement("canvas")
		canvas.width = Math.max(Math.round(video.videoWidth * scale), 1)
		canvas.height = Math.max(Math.round(video.videoHeight * scale), 1)
		const ctx = canvas.getContext("2d")
		if (!ctx) {
			return probe
		}
		ctx.drawImage(video, 0, 0, canvas.width, canvas.height)
		const blob = await new Promise<Blob | null>(resolve =>
			canvas.toBlob(resolve, "image/jpeg", THUMBNAIL_QUALITY))
		if (blob) {
			probe.thumbnail = await blob.bytes()
		}
		return probe
	} finally {
		video.src = ""
		URL.revokeObjectURL(url)
	}
}

async function probeAudio(file: Blob, wantWaveform: boolean): Promise<MediaProbe> {
	const audioData = await file.arrayBuffer()
	const audioCtx = new AudioContext()
	try {
		const decoded = await audioCtx.decodeAudioData(audioData)
		const probe: MediaProbe = { duration_ms: Math.round(decoded.duration * 1000) }
		if (!wantWaveform) {
			return probe
		}
		// Same bucket count the server build derives from the duration.
		const buckets = Math.min(
			Math.max(Math.floor((probe.duration_ms ?? 0) / 125), WAVEFORM_MIN_BUCKETS),
			WAVEFORM_MAX_BUCKETS,
		)
		const samples = decoded.getChannelData(0)
		const perBucket = Math.max(Math.floor(samples.length / buckets), 1)
		const peaks: number[] = []
		let loudest = 0
		for (let i = 0; i < buckets; i++) {
			let peak = 0
			const start = i * perBucket
			for (let j = start; j < start + perBucket && j < samples.length; j++) {
				const value = Math.abs(samples[j])
				if (value > peak) {
					peak = value
				}
			}
			peaks.push(peak)
			if (peak > loudest) {
				loudest = peak
			}
		}
		// Scaled against the loudest part rather than against full scale.
		// Speech into a laptop microphone peaks far below it, which would
		// otherwise draw a flat line of zeroes and ones.
		probe.waveform = peaks.map(peak => loudest > 0
			? Math.min(Math.round((peak / loudest) * WAVEFORM_MAX), WAVEFORM_MAX)
			: 0)
		return probe
	} finally {
		await audioCtx.close()
	}
}

// probeMedia never throws: everything it produces is optional extra detail, so
// a file the browser can't decode is uploaded without it rather than failing.
export async function probeMedia(file: Blob, voiceMessage: boolean): Promise<MediaProbe> {
	try {
		if (file.type.startsWith("video/")) {
			return await probeVideo(file)
		} else if (file.type.startsWith("audio/")) {
			return await probeAudio(file, voiceMessage)
		}
	} catch (err) {
		console.warn("Failed to read media details before upload", err)
	}
	return {}
}
