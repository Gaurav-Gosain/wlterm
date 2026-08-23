package main

import "testing"

// A terminal's graphics replies arrive as APC strings, and until the probe
// needed to read them there was no APC case in the parser at all: "ESC _" fell
// through to the legacy alt+key path, so a reply became "alt+_" for the guest
// followed by the rest of it as plain text. These pin both halves: the reply is
// read, and nothing of it reaches the guest.

// drain empties the answer channel.
func drainAnswers(p *inputParser) []gfxAnswer {
	var got []gfxAnswer
	for {
		select {
		case a := <-p.gfxAnswers:
			got = append(got, a)
		default:
			return got
		}
	}
}

func TestGraphicsRepliesAreReadNotTyped(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  []gfxAnswer
	}{
		{
			name:  "accepted frame edit",
			input: "\x1b_Gi=91,r=1;OK\x1b\\",
			want:  []gfxAnswer{{id: 91, ok: true}},
		},
		{
			name:  "refused frame edit",
			input: "\x1b_Gi=92,r=1;EINVAL:Frame width 9 larger than image width: 4\x1b\\",
			want:  []gfxAnswer{{id: 92, ok: false}},
		},
		{
			name:  "both, back to back",
			input: "\x1b_Gi=91,r=1;OK\x1b\\\x1b_Gi=92,r=1;EINVAL:no\x1b\\",
			want:  []gfxAnswer{{id: 91, ok: true}, {id: 92, ok: false}},
		},
		{
			name:  "an APC that is not ours",
			input: "\x1b_Xsomething else\x1b\\",
			want:  nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newInputParser(nil)
			p.buf = append(p.buf, tc.input...)
			p.parse()
			if len(p.buf) != 0 {
				t.Errorf("parser kept %q instead of consuming the whole APC string", p.buf)
			}
			got := drainAnswers(p)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d answers %v, want %d %v", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("answer %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestGraphicsReplyArrivingInPieces is the case a probe actually meets: a reply
// split across reads. An APC string that has not reached its terminator has to
// be held, not guessed at.
func TestGraphicsReplyArrivingInPieces(t *testing.T) {
	p := newInputParser(nil)
	reply := "\x1b_Gi=91,r=1;OK\x1b\\"
	for i := 0; i < len(reply); i++ {
		p.buf = append(p.buf, reply[i])
		p.parse()
		if i < len(reply)-1 {
			if got := drainAnswers(p); got != nil {
				t.Fatalf("answered after %d of %d bytes: %v", i+1, len(reply), got)
			}
		}
	}
	got := drainAnswers(p)
	if len(got) != 1 || got[0] != (gfxAnswer{id: 91, ok: true}) {
		t.Fatalf("got %v, want one accepted answer for id 91", got)
	}
}
