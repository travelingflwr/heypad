package main

import (
	"encoding/json"
	"testing"
)

// seenBox builds postings, true for each one HEY flags seen.
func seenBox(seen ...bool) []Posting {
	var out []Posting
	for i, s := range seen {
		out = append(out, Posting{ID: int64(i + 1), Seen: s})
	}
	return out
}

func TestNewCount(t *testing.T) {
	cases := []struct {
		name  string
		posts []Posting
		want  int
	}{
		{"both sections", seenBox(false, false, false, true, true), 3},
		{"all caught up", seenBox(true, true, true), 0},
		{"all new", seenBox(false, false), 2},
		{"empty", nil, 0},
		// Only the top run counts; a stray flag further down doesn't.
		{"first seen wins", seenBox(false, true, false, true), 1},
	}
	for _, c := range cases {
		if got := newCount(c.posts); got != c.want {
			t.Errorf("%s: newCount = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestPostingSeenJSON(t *testing.T) {
	// HEY's shape: Previously Seen carries "seen": true; New For You omits it.
	var ps []Posting
	if err := json.Unmarshal([]byte(`[{"id":1},{"id":2,"seen":true}]`), &ps); err != nil {
		t.Fatal(err)
	}
	if ps[0].Seen || !ps[1].Seen {
		t.Errorf("Seen = %v, %v; want false, true", ps[0].Seen, ps[1].Seen)
	}
}
