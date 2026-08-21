package main

// Frecency: the blend of frequency and recency that makes a launcher feel
// like it knows you. Without it, "fi" opens whatever sorts first among the
// things starting with "fi"; with it, "fi" opens the thing you open.
//
// The format is deliberately dull, because a launcher that corrupts its own
// state file is worse than one with no memory: one tab-separated line per
// entry, `id<TAB>count<TAB>last-unix-seconds`, written atomically through a
// temporary file and a rename. An unparsable line is skipped, not fatal.
//
// The store lives under $XDG_STATE_HOME (wlterm's own, not the private
// runtime dir handed to children), so a launch inside wlterm is remembered
// across runs but nothing about it leaks into a guest's environment.

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type frecencyRecord struct {
	count int
	last  time.Time
}

type frecencyStore struct {
	mu   sync.Mutex
	path string
	recs map[string]frecencyRecord
}

func statePath() string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		if h, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(h, ".local", "state")
		}
	}
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "wlterm", "frecency")
}

func loadFrecency() *frecencyStore {
	s := &frecencyStore{path: statePath(), recs: map[string]frecencyRecord{}}
	if s.path == "" {
		return s
	}
	f, err := os.Open(s.path)
	if err != nil {
		return s
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Split(sc.Text(), "\t")
		if len(parts) != 3 {
			continue
		}
		n, err1 := strconv.Atoi(parts[1])
		t, err2 := strconv.ParseInt(parts[2], 10, 64)
		if err1 != nil || err2 != nil || parts[0] == "" || n <= 0 {
			continue
		}
		s.recs[parts[0]] = frecencyRecord{count: n, last: time.Unix(t, 0)}
	}
	logf("frecency: %d entries from %s", len(s.recs), s.path)
	return s
}

// record counts one launch and persists immediately. A launcher is used a
// handful of times a minute at most, so there is no reason to batch, and
// writing now means a crash never loses the thing you just ran.
func (s *frecencyStore) record(id string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	r := s.recs[id]
	r.count++
	r.last = time.Now()
	s.recs[id] = r
	s.mu.Unlock()
	// Persist off the caller's goroutine: record() is reached from a key
	// press with the compositor lock held, and a disk write there would be
	// a stall in the input path.
	go func() {
		if err := s.save(); err != nil {
			logf("frecency save: %v", err)
		}
	}()
}

func (s *frecencyStore) save() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	s.mu.Lock()
	ids := make([]string, 0, len(s.recs))
	for id := range s.recs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var b strings.Builder
	for _, id := range ids {
		r := s.recs[id]
		fmt.Fprintf(&b, "%s\t%d\t%d\n", id, r.count, r.last.Unix())
	}
	s.mu.Unlock()

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// boost is the frecency contribution for one entry, on the same scale as a
// match score.
//
// Recency is bucketed rather than exponential, the way Mozilla's original
// frecency is: buckets are legible in a debug log and stable to reason
// about, and the difference between "yesterday" and "the day before" should
// not matter. Frequency enters logarithmically, so the tenth launch counts
// for much less than the second.
func (s *frecencyStore) boost(id string, now time.Time) int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	r, ok := s.recs[id]
	s.mu.Unlock()
	if !ok || r.count <= 0 {
		return 0
	}
	age := now.Sub(r.last)
	var w float64
	switch {
	case age < time.Hour:
		w = 4
	case age < 24*time.Hour:
		w = 3
	case age < 7*24*time.Hour:
		w = 2
	case age < 30*24*time.Hour:
		w = 1.5
	default:
		w = 1
	}
	return int(10 * w * math.Log2(1+float64(r.count)))
}
