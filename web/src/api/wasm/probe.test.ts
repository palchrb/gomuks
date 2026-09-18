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
import { expect, suite, test } from "vitest"
import { computeWaveform } from "./probe.ts"

// The values have to match what the server build produces with ffmpeg, so
// these tests encode that algorithm rather than what looks best: the peak of
// each bucket against full scale, averaged between the two halves of the
// wave, then scaled up so the loudest bucket reaches the top.
function synth(length: number, fill: (i: number) => number): Float32Array {
	const samples = new Float32Array(length)
	for (let i = 0; i < length; i++) {
		samples[i] = fill(i)
	}
	return samples
}

suite("computeWaveform", () => {
	test("returns one value per bucket", () => {
		expect(computeWaveform(synth(10_000, () => 0.5), 40)).toHaveLength(40)
		expect(computeWaveform(synth(10_000, () => 0.5), 120)).toHaveLength(120)
	})

	test("the loudest bucket reaches the top of the range", () => {
		const waveform = computeWaveform(synth(10_000, i => Math.sin(i / 8) * 0.025), 40)
		expect(Math.max(...waveform)).toBe(256)
	})

	test("nothing is scaled down when a bucket is already at full scale", () => {
		// A bucket at full scale means the recording is drawn at full height
		// and the server build leaves the values alone.
		const length = 10_000
		const waveform = computeWaveform(
			synth(length, i => (i > length - 200 ? 1 : Math.sin(i / 8) * 0.05)),
			40,
		)
		expect(waveform.at(-1)).toBe(256)
		expect(Math.max(...waveform.slice(0, -1))).toBeLessThan(40)
	})

	test("relative loudness is preserved", () => {
		const length = 12_000
		const waveform = computeWaveform(
			synth(length, i => Math.sin(i / 8) * (i < length / 2 ? 0.02 : 0.2)),
			40,
		)
		// Ten times louder in the second half, and the loudest is at the top.
		expect(waveform[35]).toBe(256)
		expect(waveform[5]).toBeGreaterThan(15)
		expect(waveform[5]).toBeLessThan(40)
	})

	test("both halves of the wave count", () => {
		// Only positive excursions, so the average of up and down halves it.
		const positive = computeWaveform(synth(10_000, i => (i % 2 === 0 ? 1 : 0)), 40)
		expect(positive.every(value => value === 256)).toBe(true)
	})

	test("silence is all zeroes rather than a division by zero", () => {
		const waveform = computeWaveform(synth(10_000, () => 0), 40)
		expect(waveform).toHaveLength(40)
		expect(waveform.every(value => value === 0)).toBe(true)
	})

	test("values stay within the range the spec uses", () => {
		const waveform = computeWaveform(synth(10_000, i => (i % 3 === 0 ? 1 : -1)), 40)
		expect(waveform.every(value => value >= 0 && value <= 256)).toBe(true)
	})
})
