package main

import "testing"

// wl_output's mode carries the refresh rate a client paces itself from.
// Chromium, mpv, SDL and nested compositors all read it, so it has to be the
// rate wlterm actually draws at. It was always 60 Hz, whatever -fps was.
func TestOutputModeReportsTheFrameCap(t *testing.T) {
	const (
		registry = 2
		output   = 3
		evMode   = 1
	)
	for _, tc := range []struct {
		fps  int
		want int32
	}{
		{fps: 120, want: 120000},
		{fps: 30, want: 30000},
		{fps: 0, want: 60000}, // uncapped: no rate to report, so the usual one
	} {
		comp := newSingleComp(400, 200, 10, 20)
		comp.maxFPS = tc.fps
		_, peer, _ := connect(t, comp)
		send(t, peer,
			request(1, 1, uint32(registry)),
			request(registry, 0, uint32(4), "wl_output", uint32(3), uint32(output)))
		var got []int32
		for _, e := range roundTrip(t, peer, 10) {
			if e.obj == output && e.opcode == evMode { // flags, width, height, refresh
				got = append(got, int32(e.u32(3)))
			}
		}
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("-fps %d: wl_output.mode refresh = %v mHz, want [%d]", tc.fps, got, tc.want)
		}
	}
}
