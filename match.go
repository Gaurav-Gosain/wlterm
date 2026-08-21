package main

// The matcher seam.
//
// A scoring fuzzy matcher is being built in the tuios repo and exported
// under pkg/ precisely so that this file does not become a fourth private
// implementation. Everything the launcher knows about matching goes through
// one interface with one method:
//
//	type Matcher interface {
//	    Match(query, candidate string) (Match, bool)
//	}
//
// Swapping in the shared one is a one-file change and nothing outside this
// file needs to know:
//
//	1. go.mod: require github.com/Gaurav-Gosain/tuios (+ a replace
//	   directive while it is unreleased).
//	2. Add an adapter in this file:
//
//	     type sharedMatcher struct{ m *fuzzy.Matcher }
//	     func (s sharedMatcher) Match(q, c string) (Match, bool) {
//	         r, ok := s.m.Match(q, c)
//	         return Match{Score: r.Score, Positions: r.Positions}, ok
//	     }
//
//	3. Change the one assignment below.
//
// The contract the shared matcher has to satisfy is the one this file's
// tests assert (match_test.go): higher score is a better match, Positions
// are indices into candidate identifying the matched characters, and a
// query that is not a subsequence of the candidate returns ok=false. Nothing
// depends on the absolute magnitude of a score, only on its ordering, so a
// differently-scaled scorer drops straight in.
//
// Until then, matcherImpl is a placeholder in the fzf-v2 shape: a small
// dynamic program over (query x candidate) that rewards matches at word
// boundaries and runs of consecutive characters, and penalises gaps. It is
// deliberately not clever; it is here to be replaced.

import "strings"

// Match is one candidate's result. Positions index into the candidate
// string, in increasing order.
type Match struct {
	Score     int
	Positions []int
}

// Matcher is the only thing the launcher knows about fuzzy matching.
type Matcher interface {
	Match(query, candidate string) (Match, bool)
}

// activeMatcher is the single assignment to change when the shared matcher
// lands.
var activeMatcher Matcher = &placeholderMatcher{}

// ---- placeholder ----

// Scoring constants, in the spirit of fzf's. Only the ratios matter.
const (
	scoreMatch       = 16
	bonusBoundary    = 8 // first character, or one following a separator
	bonusCamel       = 7 // lower-to-upper transition
	bonusConsecutive = 8 // immediately after the previous matched character
	bonusFirstMult   = 2 // the first query character's boundary bonus is doubled
	penaltyGapStart  = -3
	penaltyGapExtend = -1
	bonusExactCase   = 1
)

type placeholderMatcher struct {
	// Reused across calls; the launcher matches on one goroutine.
	score []int32
	pred  []int32 // column in the previous row this cell came from
	bonus []int32
	lower []byte
	carry []int32
	cfrom []int32
}

const noMatch = int32(-1 << 30)

func (p *placeholderMatcher) Match(query, candidate string) (Match, bool) {
	if query == "" {
		return Match{}, true
	}
	n, m := len(candidate), len(query)
	if m > n || n == 0 {
		return Match{}, false
	}

	// Cheap subsequence prefilter: most candidates die here, so the DP only
	// runs on the handful that could possibly match.
	if !isSubsequenceFold(query, candidate) {
		return Match{}, false
	}

	if cap(p.lower) < n {
		p.lower = make([]byte, n)
		p.bonus = make([]int32, n)
		p.carry = make([]int32, n)
		p.cfrom = make([]int32, n)
	}
	lower, bonus := p.lower[:n], p.bonus[:n]
	carry, cfrom := p.carry[:n], p.cfrom[:n]
	for i := 0; i < n; i++ {
		lower[i] = lowerByte(candidate[i])
		bonus[i] = boundaryBonus(candidate, i)
	}

	need := n * m
	if cap(p.score) < need {
		p.score = make([]int32, need)
		p.pred = make([]int32, need)
	}
	score, pred := p.score[:need], p.pred[:need]
	for i := range score {
		score[i] = noMatch
	}

	for qi := 0; qi < m; qi++ {
		qc := lowerByte(query[qi])
		row, prev := qi*n, (qi-1)*n
		// carry[j] is the best score in the previous row at any column
		// k <= j-2, already charged the gap it has travelled. A jump of
		// exactly one column is not a gap; that is the consecutive case,
		// which is scored separately and rewarded rather than penalised.
		for j := 0; j < n; j++ {
			if qi > 0 && j >= 2 {
				open, of := noMatch, int32(-1)
				if score[prev+j-2] > noMatch {
					open, of = score[prev+j-2]+penaltyGapStart, int32(j-2)
				}
				extend, ef := noMatch, int32(-1)
				if j >= 3 && carry[j-1] > noMatch {
					extend, ef = carry[j-1]+penaltyGapExtend, cfrom[j-1]
				}
				if extend > open {
					carry[j], cfrom[j] = extend, ef
				} else {
					carry[j], cfrom[j] = open, of
				}
			} else {
				carry[j], cfrom[j] = noMatch, -1
			}
			if lower[j] != qc {
				continue
			}
			var s int32
			var from int32 = -1
			if qi == 0 {
				s = scoreMatch + bonus[j]*bonusFirstMult
			} else {
				consec, jump := noMatch, noMatch
				if j > 0 && score[prev+j-1] > noMatch {
					cb := bonus[j]
					if cb < bonusConsecutive {
						cb = bonusConsecutive
					}
					consec = score[prev+j-1] + scoreMatch + cb
				}
				if carry[j] > noMatch {
					jump = carry[j] + scoreMatch + bonus[j]
				}
				if consec >= jump {
					if consec <= noMatch {
						continue
					}
					s, from = consec, int32(j-1)
				} else {
					s, from = jump, cfrom[j]
				}
			}
			if query[qi] == candidate[j] {
				s += bonusExactCase
			}
			score[row+j], pred[row+j] = s, from
		}
	}

	last := (m - 1) * n
	bestJ, bestS := -1, noMatch
	for j := 0; j < n; j++ {
		if score[last+j] > bestS {
			bestS, bestJ = score[last+j], j
		}
	}
	if bestJ < 0 || bestS <= noMatch {
		return Match{}, false
	}

	pos := make([]int, m)
	j := bestJ
	for qi := m - 1; qi >= 0; qi-- {
		pos[qi] = j
		if qi == 0 {
			break
		}
		k := pred[qi*n+j]
		if k < 0 {
			return Match{}, false
		}
		j = int(k)
	}

	// A short candidate that matches is a better answer than a long one:
	// "gc" should land on "gcc" before "gnome-calculator". The DP already
	// prefers the tighter alignment; this keeps the preference when two
	// alignments are equally tight.
	return Match{Score: int(bestS) - len(candidate)/4, Positions: pos}, true
}

func lowerByte(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + 32
	}
	return b
}

func isSubsequenceFold(q, c string) bool {
	i := 0
	for j := 0; j < len(c) && i < len(q); j++ {
		if lowerByte(c[j]) == lowerByte(q[i]) {
			i++
		}
	}
	return i == len(q)
}

// boundaryBonus rewards the character positions a person is most likely to
// be aiming at: the start of the string, the start of a word, and the
// capital in a camelCase name.
func boundaryBonus(s string, i int) int32 {
	if i == 0 {
		return bonusBoundary
	}
	prev, cur := s[i-1], s[i]
	if isSeparator(prev) {
		return bonusBoundary
	}
	if prev >= 'a' && prev <= 'z' && cur >= 'A' && cur <= 'Z' {
		return bonusCamel
	}
	if !isAlnum(prev) && isAlnum(cur) {
		return bonusBoundary
	}
	return 0
}

func isSeparator(b byte) bool { return strings.IndexByte(" \t/\\_-.,:;()[]{}", b) >= 0 }

func isAlnum(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}
