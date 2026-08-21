package main

// Compositor keybindings, in tuios's shape: a prefix key, then a command
// key. The prefix defaults to ctrl+b, the same leader tuios uses, and the
// command set is tuios's prefix table wherever the action exists here.
// Pressing the prefix twice sends it through to the guest.

import "strings"

// prefixFromName parses "ctrl+b" style names into an evdev code plus the
// kitty modifier bits that must accompany it.
func prefixFromName(name string) (code uint32, mods uint32, ok bool) {
	parts := strings.Split(strings.ToLower(name), "+")
	if len(parts) == 0 {
		return 0, 0, false
	}
	key := parts[len(parts)-1]
	for _, m := range parts[:len(parts)-1] {
		switch m {
		case "ctrl", "control", "c":
			mods |= 4
		case "alt", "meta", "m":
			mods |= 2
		case "shift", "s":
			mods |= 1
		case "super", "cmd":
			mods |= 8
		}
	}
	if len(key) != 1 {
		return 0, 0, false
	}
	code, ok = codeToEvdev[uint32(key[0])]
	return code, mods, ok
}

// handleKey intercepts compositor bindings. It returns true when the key was
// consumed and must not reach the guest.
//
// Caller holds comp.mu. event: 1 press, 3 release.
func (comp *compositor) handleKey(code, mods uint32, event int) bool {
	if comp.prefixCode == 0 {
		return false
	}
	isPrefix := code == comp.prefixCode && mods&comp.prefixMods == comp.prefixMods

	if event == 3 {
		if comp.swallow[code] {
			delete(comp.swallow, code)
			return true
		}
		return false
	}

	if !comp.prefixArmed {
		if isPrefix {
			comp.prefixArmed = true
			comp.arm(code)
			comp.chromeDirty = true
			comp.markDirty()
			return true
		}
		return false
	}

	// Armed: this key is a command.
	comp.prefixArmed = false
	comp.chromeDirty = true
	comp.markDirty()
	if isPrefix {
		// Prefix twice: the guest wanted it after all.
		return false
	}
	comp.arm(code)
	comp.runCommand(code, mods)
	return true
}

func (comp *compositor) arm(code uint32) {
	if comp.swallow == nil {
		comp.swallow = map[uint32]bool{}
	}
	comp.swallow[code] = true
}

// evdev codes for the command keys.
const (
	kcC     = 46
	kcN     = 49
	kcP     = 25
	kcX     = 45
	kcZ     = 44
	kcQ     = 16
	kcR     = 19
	kcSpace = 57
	kcTab   = 15
	kcMinus = 12
	kcEqual = 13
	kcPipe  = 43 // backslash / pipe
	kcComma = 51
	kcDot   = 52
	kcH     = 35
	kcJ     = 36
	kcK     = 37
	kcL     = 38
	kcLeft  = 105
	kcRight = 106
	kcUp    = 103
	kcDown  = 108
	kc1     = 2
)

func (comp *compositor) runCommand(code, mods uint32) {
	shift := mods&1 != 0
	switch code {
	case kcC:
		comp.launch()
	case kcN, kcTab:
		if shift {
			comp.cycleFocus(-1)
		} else {
			comp.cycleFocus(1)
		}
	case kcP:
		comp.cycleFocus(-1)
	case kcX:
		comp.closeFocused()
	case kcZ:
		comp.toggleZoom()
	case kcSpace:
		comp.toggleLayout()
	case kcMinus:
		comp.nextSplit = splitHorizontal
		comp.launch()
	case kcPipe:
		comp.nextSplit = splitVertical
		comp.launch()
	case kcEqual:
		comp.equalize()
	case kcR:
		comp.rotateSplit()
	case kcComma:
		comp.resizeSplit(-0.05)
	case kcDot:
		comp.resizeSplit(0.05)
	case kcH, kcLeft:
		comp.focusDir(-1, 0)
	case kcL, kcRight:
		comp.focusDir(1, 0)
	case kcK, kcUp:
		comp.focusDir(0, -1)
	case kcJ, kcDown:
		comp.focusDir(0, 1)
	case kcQ:
		comp.requestQuit()
	default:
		// Digits select a tile, tuios-style.
		if code >= kc1 && code < kc1+9 {
			idx := int(code - kc1)
			if idx < len(comp.windows) {
				comp.setFocus(comp.windows[idx])
			}
		}
	}
}

func (comp *compositor) launch() {
	if comp.spawn != nil && len(comp.spawnCmd) > 0 {
		comp.spawn(comp.spawnCmd)
	}
}

func (comp *compositor) requestQuit() {
	if comp.quitFn != nil {
		comp.quitFn()
	}
}
