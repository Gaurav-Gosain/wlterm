package main

import (
	"fmt"
	"testing"
)

// SGR mouse reports from the host terminal have to reach the guest as the
// button or the scroll axis the user actually used. Two used to arrive as
// something else: the back and forward buttons (SGR 128 and 129) carry none
// of the low bits, so they decoded as a left click, and pressing "back" in a
// guest Chromium clicked at the pointer. Horizontal wheel codes (66, 67) took
// the wheel branch and scrolled vertically.
func TestSGRMouseButtonsAndWheelsReachTheGuest(t *testing.T) {
	const (
		registry = 2
		seat     = 3
		pointer  = 4
		surface  = 20

		evButton = 3
		evAxis   = 4
	)
	type want struct {
		opcode uint16
		button uint32 // wl_pointer.button: the evdev code
		axis   uint32 // wl_pointer.axis: 0 vertical, 1 horizontal
		sign   int    // wl_pointer.axis: direction of the value
	}
	cases := []struct {
		name string
		code int
		want want
	}{
		{"left", 0, want{opcode: evButton, button: 0x110}},        // BTN_LEFT
		{"middle", 1, want{opcode: evButton, button: 0x112}},      // BTN_MIDDLE
		{"right", 2, want{opcode: evButton, button: 0x111}},       // BTN_RIGHT
		{"back", 128, want{opcode: evButton, button: 0x113}},      // BTN_SIDE
		{"forward", 129, want{opcode: evButton, button: 0x114}},   // BTN_EXTRA
		{"button 10", 130, want{opcode: evButton, button: 0x115}}, // BTN_FORWARD
		{"button 11", 131, want{opcode: evButton, button: 0x116}}, // BTN_BACK
		{"wheel up", 64, want{opcode: evAxis, axis: 0, sign: -1}},
		{"wheel down", 65, want{opcode: evAxis, axis: 0, sign: 1}},
		{"wheel left", 66, want{opcode: evAxis, axis: 1, sign: -1}},
		{"wheel right", 67, want{opcode: evAxis, axis: 1, sign: 1}},
		{"ctrl+wheel down", 65 | 16, want{opcode: evAxis, axis: 0, sign: 1}},
		{"shift+back", 128 | 4, want{opcode: evButton, button: 0x113}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			comp := newSingleComp(400, 200, 10, 20)
			c, peer, _ := connect(t, comp)
			send(t, peer,
				request(1, 1, uint32(registry)),
				request(registry, 0, uint32(3), "wl_seat", uint32(5), uint32(seat)),
				request(seat, 0, uint32(pointer)))
			roundTrip(t, peer, 10)
			mapSurfaceFor(comp, c, surface)

			p := newInputParser(comp)
			p.buf = append(p.buf, fmt.Sprintf("\x1b[<%d;5;5M\x1b[<%d;5;5m", tc.code, tc.code)...)
			p.parse()
			evs := roundTrip(t, peer, 11)

			var got []wireEvent
			for _, e := range evs {
				if e.obj == pointer && (e.opcode == evButton || e.opcode == evAxis) {
					got = append(got, e)
				}
			}
			if len(got) == 0 {
				t.Fatalf("SGR %d reached the guest as no button and no axis event", tc.code)
			}
			for _, e := range got {
				if e.opcode != tc.want.opcode {
					t.Fatalf("SGR %d reached the guest as wl_pointer opcode %d, want %d",
						tc.code, e.opcode, tc.want.opcode)
				}
				switch e.opcode {
				case evButton: // serial, time, button, state
					if b := e.u32(2); b != tc.want.button {
						t.Errorf("SGR %d pressed button 0x%x, want 0x%x", tc.code, b, tc.want.button)
					}
				case evAxis: // time, axis, value
					if a := e.u32(1); a != tc.want.axis {
						t.Errorf("SGR %d scrolled axis %d, want %d", tc.code, a, tc.want.axis)
					}
					v := int32(e.u32(2))
					if (v < 0) != (tc.want.sign < 0) || v == 0 {
						t.Errorf("SGR %d scrolled by %d, want the sign %d", tc.code, v, tc.want.sign)
					}
				}
			}
			if tc.want.opcode == evButton && len(got) != 2 {
				t.Errorf("SGR %d gave %d button events, want a press and a release", tc.code, len(got))
			}
		})
	}
}
