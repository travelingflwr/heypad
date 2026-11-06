package main

import (
	"encoding/base64"
	"testing"
	"time"
)

// box builds postings whose ObservedAt times are the given hours.
func observedBox(hours ...int) []Posting {
	base := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	var out []Posting
	for i, h := range hours {
		out = append(out, Posting{ID: int64(i + 1), ObservedAt: base.Add(time.Duration(h) * time.Hour)})
	}
	return out
}

func TestNewCount(t *testing.T) {
	cases := []struct {
		name  string
		posts []Posting
		next  string
		want  int
	}{
		// New For You (20, 18, 15), then Previously Seen starting over at 22.
		{"both sections", observedBox(20, 18, 15, 22, 21, 9), "seen", 3},
		// Inbox zero: the list starts in Previously Seen, one descending run.
		// Its order alone can't tell; the cursor says the next page is seen.
		{"all caught up", observedBox(22, 21, 9, 4), "seen", 0},
		// Everything fetched is new, and the next page is new too.
		{"all new, more to fetch", observedBox(20, 18, 15, 3), "unseen", 4},
		// A tiny box with no next page and no break: nothing to call new.
		{"small box, no cursor", observedBox(5, 4), "", 0},
		{"empty", nil, "", 0},
		// Only the first rise is the boundary; Previously Seen is ordered too.
		{"first rise wins", observedBox(10, 8, 12, 11, 2), "seen", 2},
	}
	for _, c := range cases {
		if got := newCount(c.posts, c.next); got != c.want {
			t.Errorf("%s: newCount = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestCursorSection(t *testing.T) {
	// A real cursor from HEY's Imbox.
	real := "eyJwYWdlX251bWJlciI6MTksInZhbHVlcyI6eyJzZWVuIjoic2VlbiIsIm9ic2VydmVkX2F0IjoiMjAyNi0xMS0wM1QwMDo0NTo1My42ODk4NjNaIiwiaWQiOjEyNzQ3MzY2NzF9fQ=="
	if got := cursorSection(real); got != "seen" {
		t.Errorf("cursorSection(real) = %q, want seen", got)
	}
	unseen := base64.RawURLEncoding.EncodeToString([]byte(`{"page_number":2,"values":{"seen":"unseen"}}`))
	if got := cursorSection(unseen); got != "unseen" {
		t.Errorf("cursorSection(unpadded) = %q, want unseen", got)
	}
	if got := cursorSection("not a cursor"); got != "" {
		t.Errorf("cursorSection(junk) = %q, want empty", got)
	}
}

func TestLayoutListSections(t *testing.T) {
	posts := observedBox(20, 18, 22)
	posts[0].New, posts[1].New = true, true
	l := layoutList(posts, true)
	if len(l.heads) != 2 || l.heads[0].title != "New For You" || l.heads[0].count != 2 || l.heads[1].count != 1 {
		t.Fatalf("heads = %+v", l.heads)
	}
	if !(l.tops[0] > l.heads[0].y && l.tops[2] > l.heads[1].y && l.heads[1].y > l.tops[1]) {
		t.Errorf("rows and headings out of order: tops %v, heads %+v", l.tops, l.heads)
	}
	if got := layoutList(posts, false); len(got.heads) != 0 || got.tops[1]-got.tops[0] != rowPitch {
		t.Errorf("without sections: %+v", got)
	}
	if i := l.rowAt(l.tops[2] + 1); i != 2 {
		t.Errorf("rowAt = %d, want 2", i)
	}
}

func TestCleanBody(t *testing.T) {
	got := cleanBody("Hi &amp; hello [click](https://x.example)\n\n![img](https://x/y.png)\n> quoted\n- item\n| a | b |\n| --- | --- |")
	want := []para{{paraText, "Hi & hello click"}, {paraQuote, "quoted"}, {paraBullet, "item"}, {paraText, "a  ·  b"}}
	if len(got) != len(want) {
		t.Fatalf("got %d paragraphs %+v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("paragraph %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
