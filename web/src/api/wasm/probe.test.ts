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

// Three attempts at this were wrong in ways only visible in a sent message:
// scaled against full scale it was a flat line of ones, scaled against the
// loudest sample a single click at the end flattened everything else.
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

	test("quiet speech still uses most of the range", () => {
		// Peaks at a fortieth of full scale, which scaled absolutely would be
		// a row of sixes.
		const waveform = computeWaveform(synth(10_000, i => Math.sin(i / 8) * 0.025), 40)
		expect(Math.max(...waveform)).toBeGreaterThan(200)
	})

	test("a click at the end does not flatten the rest", () => {
		const length = 10_000
		const waveform = computeWaveform(
			// Steady speech, then a single very loud bucket at the end.
			synth(length, i => (i > length - 200 ? 1 : Math.sin(i / 8) * 0.05)),
			40,
		)
		const body = waveform.slice(0, -1)
		expect(Math.max(...body)).toBeGreaterThan(150)
		expect(waveform.at(-1)).toBe(256)
	})

	test("louder passages read higher than quieter ones", () => {
		const length = 12_000
		const waveform = computeWaveform(
			synth(length, i => Math.sin(i / 8) * (i < length / 2 ? 0.02 : 0.2)),
			40,
		)
		expect(waveform[5]).toBeLessThan(waveform[35])
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
