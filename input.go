package main

// Terminal input -> Wayland seat events.
// Understands the kitty keyboard protocol (CSI u with event types) and SGR
// mouse (cell 1006 or pixel 1016), with a legacy fallback that synthesizes
// press+release pairs.

import (
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	btnLeft   = 0x110
	btnRight  = 0x111
	btnMiddle = 0x112
)

// quitWindow is how close together the two taps of the escape hatch have to
// be. kcBackslash is evdev KEY_BACKSLASH.
const (
	quitWindow  = 700 * time.Millisecond
	kcBackslash = 43
)

type inputParser struct {
	comp       *compositor
	buf        []byte
	pixelMouse bool
	kittyKB    bool
	headless   bool
	lastQuit   time.Time
	quit       chan struct{}

	// quitKey is the evdev code of the escape hatch, tapped twice inside
	// quitWindow. Zero means no key is intercepted at all and every byte
	// that arrives reaches the guest, which is the single-app default.
	quitKey  uint32
	quitMods uint32
	quitName string

	// responses to startup queries land here
	cellW, cellH int
	textW, textH int
	gotCell      chan struct{}
	decrqm1016   chan bool
	kittyKBProbe chan bool
}

func newInputParser(comp *compositor) *inputParser {
	return &inputParser{
		comp:         comp,
		quit:         make(chan struct{}),
		gotCell:      make(chan struct{}, 2),
		decrqm1016:   make(chan bool, 1),
		kittyKBProbe: make(chan bool, 1),
	}
}

func (p *inputParser) run() {
	rb := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(rb)
		if err != nil {
			if !p.headless {
				close(p.quit)
			}
			return
		}
		p.buf = append(p.buf, rb[:n]...)
		p.parse()
	}
}

func (p *inputParser) parse() {
	for len(p.buf) > 0 {
		if p.buf[0] != 0x1b {
			// legacy plain byte
			b := p.buf[0]
			p.buf = p.buf[1:]
			p.legacyByte(b)
			continue
		}
		if len(p.buf) == 1 {
			return // lone ESC, wait for more
		}
		if p.buf[1] == '[' {
			consumed := p.parseCSI(p.buf[2:])
			if consumed < 0 {
				return // incomplete
			}
			p.buf = p.buf[2+consumed:]
			continue
		}
		if p.buf[1] == 'O' && len(p.buf) >= 3 {
			// SS3: F1-F4, arrows in app mode
			if code, ok := csiLetterToEvdev[p.buf[2]]; ok {
				p.tap(code, 0)
			}
			p.buf = p.buf[3:]
			continue
		}
		// ESC + byte: alt+key (legacy)
		b := p.buf[1]
		p.buf = p.buf[2:]
		p.legacyAltByte(b)
	}
}

// parseCSI parses one CSI sequence from data (after "ESC["), returning bytes
// consumed or -1 if incomplete.
func (p *inputParser) parseCSI(data []byte) int {
	// find final byte 0x40-0x7e
	end := -1
	for i, b := range data {
		if b >= 0x40 && b <= 0x7e {
			end = i
			break
		}
		if i > 64 {
			return i + 1 // garbage, drop
		}
	}
	if end == -1 {
		return -1
	}
	body := string(data[:end])
	final := data[end]
	n := end + 1

	switch final {
	case 'M', 'm': // SGR mouse: CSI < b;x;y M/m
		if strings.HasPrefix(body, "<") {
			p.sgrMouse(body[1:], final == 'M')
		}
		return n
	case 'u':
		if strings.HasPrefix(body, "?") {
			// kitty keyboard probe response: CSI ? flags u
			select {
			case p.kittyKBProbe <- true:
			default:
			}
			return n
		}
		p.kittyKey(body)
		return n
	case 't': // XTWINOPS response: CSI 6;h;w t (cell px) or CSI 4;h;w t
		parts := strings.Split(body, ";")
		if len(parts) == 3 {
			h, _ := strconv.Atoi(parts[1])
			w, _ := strconv.Atoi(parts[2])
			switch parts[0] {
			case "6":
				p.cellW, p.cellH = w, h
			case "4":
				p.textW, p.textH = w, h
			}
			select {
			case p.gotCell <- struct{}{}:
			default:
			}
		}
		return n
	case 'y': // DECRPM: CSI ? mode;value $y
		if strings.HasPrefix(body, "?") && strings.HasSuffix(body, "$") {
			parts := strings.Split(strings.TrimSuffix(body[1:], "$"), ";")
			if len(parts) == 2 && parts[0] == "1016" {
				v, _ := strconv.Atoi(parts[1])
				select {
				case p.decrqm1016 <- v == 1 || v == 3:
				default:
				}
			}
		}
		return n
	case 'c': // DA1 response: CSI ? ... c - end of probe window
		select {
		case p.kittyKBProbe <- false:
		default:
		}
		return n
	case 'A', 'B', 'C', 'D', 'H', 'F', 'P', 'Q', 'S', 'E':
		code, ok := csiLetterToEvdev[final]
		if ok {
			mods, event, explicit := parseModsEvent(body)
			p.dispatchKeyEx(code, mods, event, explicit)
		}
		return n
	case '~':
		parts := strings.SplitN(body, ";", 2)
		num, _ := strconv.Atoi(parts[0])
		if code, ok := csiTildeToEvdev[num]; ok {
			modBody := ""
			if len(parts) == 2 {
				modBody = "1;" + parts[1]
			}
			mods, event, explicit := parseModsEvent(modBody)
			p.dispatchKeyEx(code, mods, event, explicit)
		}
		return n
	}
	return n
}

// parseModsEvent parses "1;mods:event" style parameter bodies. explicit
// reports whether an event-type subfield was actually present: a terminal
// that omits it sends press-only encodings and will never send releases, so
// the caller must synthesize them (otherwise the client's xkb auto-repeat
// runs forever; the innermost shell of the recursion demo received 252
// Enters from exactly this).
func parseModsEvent(body string) (mods uint32, event int, explicit bool) {
	event = 1
	parts := strings.Split(body, ";")
	if len(parts) < 2 {
		return 0, 1, false
	}
	me := strings.Split(parts[1], ":")
	m, _ := strconv.Atoi(me[0])
	if m > 0 {
		mods = uint32(m - 1)
	}
	if len(me) > 1 {
		event, _ = strconv.Atoi(me[1])
		if event == 0 {
			event = 1
		}
		explicit = true
	}
	return mods, event, explicit
}

// kittyKey parses "code;mods:event" or "code:alt;mods:event" CSI u bodies.
func (p *inputParser) kittyKey(body string) {
	parts := strings.Split(body, ";")
	codePart := strings.Split(parts[0], ":")
	cp, err := strconv.Atoi(codePart[0])
	if err != nil {
		return
	}
	mods := uint32(0)
	event := 1
	explicit := false
	if len(parts) >= 2 {
		mods, event, explicit = parseModsEvent("1;" + parts[1])
	}

	// The escape hatch, tapped twice quickly. The first tap still reaches
	// the guest, so a single press is not swallowed.
	if p.quitKey != 0 && event == 1 {
		if code, ok := codeToEvdev[uint32(cp)]; ok &&
			code == p.quitKey && mods&p.quitMods == p.quitMods {
			if time.Since(p.lastQuit) < quitWindow {
				close(p.quit)
				return
			}
			p.lastQuit = time.Now()
		}
	}

	code, ok := codeToEvdev[uint32(cp)]
	if !ok {
		if cp < 0x110000 {
			r := rune(cp)
			if r >= 'A' && r <= 'Z' {
				code, ok = codeToEvdev[uint32(r+32)]
				mods |= 1
			} else if c2, ok2 := shiftedToEvdev[r]; ok2 {
				code, ok = c2, true
				mods |= 1
			}
		}
	}
	if !ok {
		logf("unmapped key codepoint %d", cp)
		return
	}
	p.dispatchKeyEx(code, mods, event, explicit)
}

// dispatchKey: event 1=press 2=repeat 3=release
func (p *inputParser) dispatchKey(code uint32, kittyMods uint32, event int) {
	p.dispatchKeyEx(code, kittyMods, event, p.kittyKB)
}

// dispatchKeyEx: trustRelease says a matching release event will arrive
// later; when false a press is delivered as press+release.
func (p *inputParser) dispatchKeyEx(code uint32, kittyMods uint32, event int, trustRelease bool) {
	comp := p.comp
	comp.mu.Lock()
	defer comp.mu.Unlock()
	// Compositor bindings get first refusal on every key.
	if comp.handleKey(code, kittyMods, event) {
		if !trustRelease && event == 1 {
			delete(comp.swallow, code) // no release will arrive to swallow
		}
		return
	}
	comp.setModifiers(kittyModsToXkb(kittyMods))
	switch event {
	case 1:
		comp.key(code, true)
		if !trustRelease {
			comp.key(code, false) // no release events coming; synthesize
		}
	case 3:
		comp.key(code, false)
	}
	// event 2 (repeat): dropped, client repeats via repeat_info
}

func (p *inputParser) tap(code uint32, mods uint32) {
	p.dispatchKey(code, mods, 1)
	if p.kittyKB {
		p.dispatchKey(code, mods, 3)
	}
}

func (p *inputParser) legacyByte(b byte) {
	switch {
	case b == 0x1c: // ctrl+backslash
		if p.quitKey == kcBackslash && p.quitMods&4 != 0 {
			if time.Since(p.lastQuit) < quitWindow {
				close(p.quit)
				return
			}
			p.lastQuit = time.Now()
		}
		p.tapLegacy(kcBackslash, 4)
	case b >= 1 && b <= 26 && b != 9 && b != 13: // ctrl+letter
		p.tapLegacy(codeToEvdev[uint32('a'+b-1)], 4)
	case b == 9 || b == 13 || b == 127 || b == 27:
		p.tapLegacy(codeToEvdev[uint32(b)], 0)
	case b >= 32 && b < 127:
		r := rune(b)
		if code, ok := codeToEvdev[uint32(b)]; ok {
			p.tapLegacy(code, 0)
		} else if r >= 'A' && r <= 'Z' {
			p.tapLegacy(codeToEvdev[uint32(r+32)], 1)
		} else if code, ok := shiftedToEvdev[r]; ok {
			p.tapLegacy(code, 1)
		}
	}
}

func (p *inputParser) legacyAltByte(b byte) {
	if code, ok := codeToEvdev[uint32(b)]; ok {
		p.tapLegacy(code, 2)
	}
}

// tapLegacy sends press+release with a transient modifier state.
func (p *inputParser) tapLegacy(code uint32, kittyMods uint32) {
	if code == 0 {
		return
	}
	comp := p.comp
	comp.mu.Lock()
	defer comp.mu.Unlock()
	if comp.handleKey(code, kittyMods, 1) {
		delete(comp.swallow, code)
		return
	}
	comp.setModifiers(kittyModsToXkb(kittyMods))
	comp.key(code, true)
	comp.key(code, false)
	comp.setModifiers(0)
}

// sgrMouse handles "b;x;y" with press flag.
func (p *inputParser) sgrMouse(body string, press bool) {
	parts := strings.Split(body, ";")
	if len(parts) != 3 {
		return
	}
	b, _ := strconv.Atoi(parts[0])
	x, _ := strconv.Atoi(parts[1])
	y, _ := strconv.Atoi(parts[2])

	var px, py float64
	if p.pixelMouse {
		px, py = float64(x-1), float64(y-1)
	} else {
		cw, ch := p.comp.cellW, p.comp.cellH
		if cw == 0 {
			cw, ch = 10, 20
		}
		px = float64((x-1)*cw + cw/2)
		py = float64((y-1)*ch + ch/2)
	}

	comp := p.comp
	comp.mu.Lock()
	defer comp.mu.Unlock()

	if b&64 != 0 { // wheel
		if press {
			delta := 15.0
			if b&1 == 0 { // 64 = up
				delta = -15.0
			}
			comp.pointerMotion(px, py)
			comp.pointerAxis(true, delta)
		}
		return
	}
	comp.pointerMotion(px, py)
	if b&32 != 0 { // motion only
		return
	}
	var btn uint32
	switch b & 3 {
	case 0:
		btn = btnLeft
	case 1:
		btn = btnMiddle
	case 2:
		btn = btnRight
	case 3:
		return
	}
	comp.pointerButton(btn, press)
}
