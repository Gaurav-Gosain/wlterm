package main

// Unicode codepoint (kitty keyboard protocol "unicode key code", i.e. the
// unmodified base-layer key) -> Linux evdev keycode. wl_keyboard clients add
// 8 to get the xkb keycode. US layout only for the prototype.

var codeToEvdev = map[uint32]uint32{
	27: 1, 13: 28, 9: 15, 127: 14, 32: 57,
	'a': 30, 'b': 48, 'c': 46, 'd': 32, 'e': 18, 'f': 33, 'g': 34,
	'h': 35, 'i': 23, 'j': 36, 'k': 37, 'l': 38, 'm': 50, 'n': 49,
	'o': 24, 'p': 25, 'q': 16, 'r': 19, 's': 31, 't': 20, 'u': 22,
	'v': 47, 'w': 17, 'x': 45, 'y': 21, 'z': 44,
	'1': 2, '2': 3, '3': 4, '4': 5, '5': 6, '6': 7, '7': 8, '8': 9,
	'9': 10, '0': 11,
	'-': 12, '=': 13, '[': 26, ']': 27, ';': 39, '\'': 40, '`': 41,
	'\\': 43, ',': 51, '.': 52, '/': 53,
	// kitty functional codepoints
	57358: 58 /*caps*/, 57359: 70 /*scrolllock*/, 57360: 69, /*numlock*/
	57441: 42 /*lshift*/, 57442: 29 /*lctrl*/, 57443: 56 /*lalt*/, 57444: 125, /*lsuper*/
	57447: 54 /*rshift*/, 57448: 97 /*rctrl*/, 57449: 100 /*ralt*/, 57450: 126, /*rsuper*/
	// keypad (kitty codes)
	57399: 82, 57400: 79, 57401: 80, 57402: 81, 57403: 75, 57404: 76,
	57405: 77, 57406: 71, 57407: 72, 57408: 73, 57409: 83, 57410: 98,
	57411: 55, 57412: 74, 57413: 78, 57414: 96,
}

// shifted char -> base evdev code. Used when a terminal hands us the
// shifted glyph instead of the base key.
var shiftedToEvdev = map[rune]uint32{
	'!': 2, '@': 3, '#': 4, '$': 5, '%': 6, '^': 7, '&': 8, '*': 9,
	'(': 10, ')': 11, '_': 12, '+': 13, '{': 26, '}': 27, ':': 39,
	'"': 40, '~': 41, '|': 43, '<': 51, '>': 52, '?': 53,
}

// CSI final letter -> evdev (for CSI 1;mods X style keys)
var csiLetterToEvdev = map[byte]uint32{
	'A': 103, 'B': 108, 'C': 106, 'D': 105, // arrows
	'H': 102, 'F': 107, // home, end
	'P': 59, 'Q': 60, 'R': 61, 'S': 62, // F1-F4
	'E': 76, // kp_5 / begin
}

// CSI number ~ -> evdev
var csiTildeToEvdev = map[int]uint32{
	2: 110, 3: 111, 5: 104, 6: 109, 7: 102, 8: 107, 1: 102, 4: 107,
	11: 59, 12: 60, 13: 61, 14: 62, 15: 63, 17: 64, 18: 65, 19: 66,
	20: 67, 21: 68, 23: 87, 24: 88, 29: 127, /*menu*/
}

// kitty modifier bits (mods-1): shift 1, alt 2, ctrl 4, super 8, hyper 16,
// meta 32, capslock 64, numlock 128.
// xkb mask for our us keymap: Shift 1, Lock 2, Control 4, Mod1 8, Mod2 16,
// Mod4 64.
func kittyModsToXkb(m uint32) uint32 {
	var x uint32
	if m&1 != 0 {
		x |= 1
	}
	if m&2 != 0 {
		x |= 8
	}
	if m&4 != 0 {
		x |= 4
	}
	if m&8 != 0 {
		x |= 64
	}
	if m&64 != 0 {
		x |= 2
	}
	if m&128 != 0 {
		x |= 16
	}
	return x
}

// ---- evdev -> character, for the launcher's text field ----
//
// Every input path (kitty CSI u, SS3, legacy bytes) has been reduced to an
// evdev code by the time a compositor binding sees it, and the launcher
// needs the character back. US layout only, same caveat as the forward maps
// above: a compositor that wanted real layouts would carry the xkb state it
// already compiles for wl_keyboard instead of a table.
var evdevToChar map[uint32][2]rune // [unshifted, shifted]

func init() {
	evdevToChar = make(map[uint32][2]rune, len(codeToEvdev))
	for r, code := range codeToEvdev {
		if r < 32 || r > 126 {
			continue
		}
		e := evdevToChar[code]
		e[0] = rune(r)
		if r >= 'a' && r <= 'z' {
			e[1] = rune(r) - 32
		} else {
			e[1] = rune(r)
		}
		evdevToChar[code] = e
	}
	for r, code := range shiftedToEvdev {
		e := evdevToChar[code]
		e[1] = r
		evdevToChar[code] = e
	}
	evdevToChar[kcSpace] = [2]rune{' ', ' '}
}

// charForKey resolves a printable character, or reports that the key does
// not produce one.
func charForKey(code uint32, shift bool) (rune, bool) {
	e, ok := evdevToChar[code]
	if !ok {
		return 0, false
	}
	r := e[0]
	if shift {
		r = e[1]
	}
	if r < 32 || r > 126 {
		return 0, false
	}
	return r, true
}
