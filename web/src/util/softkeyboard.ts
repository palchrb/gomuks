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

// The bottom safe-area inset keeps the composer above the home indicator on
// phones. While the software keyboard is up it covers that area, but iOS keeps
// reporting the inset, so the page reserves the home indicator's height of
// nothing between the composer and the keyboard. CSS has no signal for the
// keyboard; the visual viewport shrinking while something editable has focus
// is the reliable one. The class only zeroes the inset, so being wrong where
// the inset is already zero (desktops, most Android phones) changes nothing.
//
// The comparison is against the screen, not the layout viewport, because a
// standalone PWA on iOS doesn't resize the layout viewport for the keyboard:
// it pushes the whole page up. The visual viewport still shrinks by the
// keyboard's height in that case, its offset just grows by the same amount,
// so neither window.innerHeight nor the offset can be part of the sum.
const KEYBOARD_MIN_SCREEN_FRACTION = 0.15

function isEditable(elem: Element | null): boolean {
	return elem instanceof HTMLElement
		&& (elem.isContentEditable || elem.tagName === "TEXTAREA" || elem.tagName === "INPUT")
}

function update() {
	const viewport = window.visualViewport
	if (!viewport) {
		return
	}
	const covered = screen.height - viewport.height
	const open = isEditable(document.activeElement) && covered > screen.height * KEYBOARD_MIN_SCREEN_FRACTION
	document.documentElement.classList.toggle("keyboard-open", open)
}

export function watchSoftKeyboard() {
	window.visualViewport?.addEventListener("resize", update)
	window.addEventListener("resize", update)
	document.addEventListener("focusin", update)
	// activeElement is still the old element during focusout.
	document.addEventListener("focusout", () => setTimeout(update, 0))
}
