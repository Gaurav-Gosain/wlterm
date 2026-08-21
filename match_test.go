package main

import (
	"sort"
	"testing"
)

// These tests are the contract the shared tuios matcher has to satisfy when
// it replaces the placeholder. They assert ordering and positions, never an
// absolute score, so a differently-scaled scorer passes unchanged.

func score(t *testing.T, q, c string) int {
	t.Helper()
	m, ok := activeMatcher.Match(q, c)
	if !ok {
		t.Fatalf("%q should match %q", q, c)
	}
	return m.Score
}

func TestNonSubsequenceDoesNotMatch(t *testing.T) {
	for _, c := range []struct{ q, s string }{
		{"xyz", "Firefox"},
		{"zq", "Chromium"},
		{"oif", "Firefox"}, // right characters, wrong order
		{"longerthanthecandidate", "vim"},
	} {
		if _, ok := activeMatcher.Match(c.q, c.s); ok {
			t.Fatalf("%q must not match %q", c.q, c.s)
		}
	}
}

func TestEmptyQueryMatchesEverything(t *testing.T) {
	m, ok := activeMatcher.Match("", "anything")
	if !ok || len(m.Positions) != 0 {
		t.Fatalf("empty query: ok=%v positions=%v", ok, m.Positions)
	}
}

func TestPositionsIdentifyMatchedCharacters(t *testing.T) {
	m, ok := activeMatcher.Match("frf", "Firefox")
	if !ok {
		t.Fatal("frf should match Firefox")
	}
	if len(m.Positions) != 3 {
		t.Fatalf("positions = %v, want one per query character", m.Positions)
	}
	for i, p := range m.Positions {
		if p < 0 || p >= len("Firefox") {
			t.Fatalf("position %d out of range: %v", i, m.Positions)
		}
		if i > 0 && p <= m.Positions[i-1] {
			t.Fatalf("positions must strictly increase: %v", m.Positions)
		}
		if lowerByte("Firefox"[p]) != lowerByte("frf"[i]) {
			t.Fatalf("position %d points at %q, not %q", p, "Firefox"[p], "frf"[i])
		}
	}
}

// Ranking is what actually matters: people press Enter without looking, so
// the right answer has to be first.
func TestRanking(t *testing.T) {
	cases := []struct {
		q      string
		better string
		worse  string
		why    string
	}{
		{"gc", "gcc", "gnome-calculator", "consecutive and short beats a long word-boundary spread"},
		{"fire", "Firefox", "Thunar File Manager - Recent", "a prefix beats a scatter"},
		{"chrom", "Chromium", "Chromium - New Incognito Window", "the plain application beats its action"},
		{"term", "Terminal", "Task Editor Room Manager", "one run beats four scattered initials"},
		{"vlc", "VLC", "Video Layer Control", "exact and short wins"},
		{"code", "Code", "Kernel Command Decoder Engine", "prefix beats initials"},
	}
	for _, c := range cases {
		b, w := score(t, c.q, c.better), score(t, c.q, c.worse)
		if b <= w {
			t.Errorf("%q: %q scored %d but %q scored %d (%s)", c.q, c.better, b, c.worse, w, c.why)
		}
	}
}

func TestConsecutiveBeatsScattered(t *testing.T) {
	if score(t, "abc", "abcdef") <= score(t, "abc", "axbxcx") {
		t.Fatal("a run of consecutive characters must outscore a scattered one")
	}
}

func TestWordBoundaryBeatsMidWord(t *testing.T) {
	if score(t, "s", "the sun") <= score(t, "s", "these") {
		t.Fatal("a match at a word boundary must outscore one mid-word")
	}
}

func TestShorterCandidateWinsOnATie(t *testing.T) {
	if score(t, "ss", "ssh") <= score(t, "ss", "ssh-askpass-and-more") {
		t.Fatal("with the same alignment, the shorter candidate is the better answer")
	}
}

// A sanity check that the whole ordering is stable and deterministic.
func TestDeterministic(t *testing.T) {
	names := []string{"Firefox", "Files", "File Roller", "Profiler", "fzf"}
	rank := func() []string {
		type sc struct {
			n string
			s int
		}
		var out []sc
		for _, n := range names {
			if m, ok := activeMatcher.Match("fil", n); ok {
				out = append(out, sc{n, m.Score})
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].s > out[j].s })
		var r []string
		for _, o := range out {
			r = append(r, o.n)
		}
		return r
	}
	a, b := rank(), rank()
	if len(a) != len(b) {
		t.Fatal("unstable result count")
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("unstable ordering: %v vs %v", a, b)
		}
	}
}
