package main

// The data layer: everything heypad knows about mail comes from the hey CLI's
// JSON output. Results are cached on disk so the viewer opens on the last
// known state at once and refreshes behind it.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Contact struct {
	Name        string `json:"name"`
	Email       string `json:"email_address"`
	Initials    string `json:"initials"`
	AvatarColor string `json:"avatar_background_color"`
}

func (c Contact) display() string {
	if c.Name != "" {
		return c.Name
	}
	return c.Email
}

// Posting is one thread as a box lists it.
type Posting struct {
	ID        int64     `json:"id"` // the box item ID that `hey seen` takes
	TopicID   int64     `json:"topic_id"`
	Subject   string    `json:"name"`
	Summary   string    `json:"summary"`
	ActiveAt  time.Time `json:"active_at"`
	Creator   Contact   `json:"creator"`
	AltSender string    `json:"alternative_sender_name"`
	Count     int       `json:"visible_entry_count"`
	Attach    bool      `json:"includes_attachments"`
	AppURL    string    `json:"app_url"`
	// ObservedAt is when the thread last landed in this box section: HEY
	// orders each section of the Imbox by it, newest first.
	ObservedAt time.Time `json:"observed_at"`
	// New is heypad's: the thread is in the Imbox's New For You section,
	// which is what HEY counts as unread.
	New bool `json:"heypad_new"`
}

func (p Posting) sender() string {
	if p.AltSender != "" {
		return p.AltSender
	}
	return p.Creator.display()
}

// Entry is one message in a thread.
type Entry struct {
	ID        int64   `json:"id"`
	CreatedAt string  `json:"created_at"` // "2006-01-02T15:04", in UTC
	Creator   Contact `json:"creator"`
	AltSender string  `json:"alternative_sender_name"`
	Summary   string  `json:"summary"`
	Body      string  `json:"body"` // Markdown, with HTML entities
	Kind      string  `json:"kind"`
}

func (e Entry) sender() string {
	if e.AltSender != "" {
		return e.AltSender
	}
	return e.Creator.display()
}

func (e Entry) when() time.Time {
	t, err := time.ParseInLocation("2006-01-02T15:04", e.CreatedAt, time.UTC)
	if err != nil {
		t, _ = time.Parse(time.RFC3339, e.CreatedAt)
	}
	return t
}

type box struct{ kind, title string }

var boxes = []box{
	{"imbox", "Imbox"},
	{"feedbox", "The Feed"},
	{"trailbox", "Paper Trail"},
	{"laterbox", "Reply Later"},
	{"asidebox", "Set Aside"},
	{"bubblebox", "Bubble Up"},
}

func heyBin() string {
	if p := os.Getenv("HEYPAD_HEY"); p != "" {
		return p
	}
	return "hey"
}

// heyJSON runs hey with --json and returns its output, or the first line of
// what it printed on failure.
func heyJSON(args ...string) ([]byte, error) {
	cmd := exec.Command(heyBin(), append(args, "--json")...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(string(out))
		}
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New(strings.SplitN(msg, "\n", 2)[0])
	}
	return out, nil
}

func stateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "heypad")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "heypad")
}

func writeCache(name string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	os.MkdirAll(stateDir(), 0o700)
	tmp := filepath.Join(stateDir(), name+".tmp")
	if os.WriteFile(tmp, data, 0o600) == nil {
		os.Rename(tmp, filepath.Join(stateDir(), name))
	}
}

func readCache(name string, v any) bool {
	data, err := os.ReadFile(filepath.Join(stateDir(), name))
	return err == nil && json.Unmarshal(data, v) == nil
}

func boxCache(kind string) string    { return "box-" + kind + ".json" }
func threadCache(topic int64) string { return fmt.Sprintf("thread-%d.json", topic) }

func cachedBox(kind string) []Posting {
	var p []Posting
	readCache(boxCache(kind), &p)
	return p
}

func cachedThread(topic int64) []Entry {
	var e []Entry
	readCache(threadCache(topic), &e)
	return e
}

// boxPage is one fetch of a box: its threads, and the section ("seen" or
// "unseen") that the next page would start in, from HEY's page cursor.
type boxPage struct {
	posts       []Posting
	nextSection string
}

func fetchBoxPage(kind string, limit int) (boxPage, error) {
	out, err := heyJSON("box", kind, "--limit", fmt.Sprint(limit))
	if err != nil {
		return boxPage{}, err
	}
	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			Postings []Posting `json:"postings"`
			NextPage string    `json:"next_page"`
		} `json:"data"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		return boxPage{}, fmt.Errorf("reading hey's reply: %w", err)
	}
	if !env.OK {
		return boxPage{}, errors.New(firstNonEmpty(env.Error, "hey reported an error"))
	}
	return boxPage{env.Data.Postings, cursorSection(env.Data.NextPage)}, nil
}

// cursorSection reads the Imbox section out of a HEY page cursor, which is
// base64 JSON like {"page_number":2,"values":{"seen":"unseen",...}}.
func cursorSection(cursor string) string {
	if cursor == "" {
		return ""
	}
	var data []byte
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding} {
		if d, err := enc.DecodeString(cursor); err == nil {
			data = d
			break
		}
	}
	var c struct {
		Values struct {
			Seen string `json:"seen"`
		} `json:"values"`
	}
	if json.Unmarshal(data, &c) != nil {
		return ""
	}
	return c.Values.Seen
}

// newCount is how many of the Imbox's first threads are in New For You.
// The Imbox lists New For You, then Previously Seen, each newest first by
// ObservedAt, so the one place that order goes back up is where Previously
// Seen begins. With no such place, everything fetched is in one section,
// and the page cursor says which (next is where the next page starts).
func newCount(posts []Posting, next string) int {
	for i := 1; i < len(posts); i++ {
		if posts[i].ObservedAt.After(posts[i-1].ObservedAt) {
			return i
		}
	}
	if next == "unseen" {
		return len(posts) // all new, and maybe more past what was fetched
	}
	return 0 // all caught up (or a box too small to have a next page)
}

const (
	seenToKeep = 20  // how much of Previously Seen to fetch
	maxNew     = 600 // the most of New For You to fetch
)

// fetchBox fetches a box. The Imbox comes back as its New For You threads,
// all of them up to maxNew and marked New, then up to seenToKeep of
// Previously Seen. hint is how many were new last time, to size the fetch.
func fetchBox(kind string, hint int) ([]Posting, error) {
	if kind != "imbox" {
		pg, err := fetchBoxPage(kind, 60)
		if err != nil {
			return nil, err
		}
		writeCache(boxCache(kind), pg.posts)
		return pg.posts, nil
	}
	limit := max(60, hint+seenToKeep+20)
	for {
		pg, err := fetchBoxPage(kind, limit)
		if err != nil {
			return nil, err
		}
		n := newCount(pg.posts, pg.nextSection)
		fetchedAll := len(pg.posts) < limit
		if n < len(pg.posts) || fetchedAll || limit >= maxNew+seenToKeep {
			posts := pg.posts[:min(len(pg.posts), n+seenToKeep)]
			for i := range posts {
				posts[i].New = i < n
			}
			writeCache(boxCache(kind), posts)
			return posts, nil
		}
		// Everything fetched was new: there's more of New For You.
		limit = min(maxNew+seenToKeep, limit*2+seenToKeep)
	}
}

func fetchThread(topic int64) ([]Entry, error) {
	out, err := heyJSON("thread", "read", fmt.Sprint(topic))
	if err != nil {
		return nil, err
	}
	var env struct {
		OK    bool    `json:"ok"`
		Data  []Entry `json:"data"`
		Error string  `json:"error"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		return nil, fmt.Errorf("reading hey's reply: %w", err)
	}
	if !env.OK {
		return nil, errors.New(firstNonEmpty(env.Error, "hey reported an error"))
	}
	writeCache(threadCache(topic), env.Data)
	return env.Data, nil
}

func markSeen(p Posting) error {
	_, err := heyJSON("seen", fmt.Sprint(p.ID))
	return err
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// --- message bodies -----------------------------------------------------

type paraKind int

const (
	paraText paraKind = iota
	paraQuote
	paraHeading
	paraBullet
)

type para struct {
	kind paraKind
	text string // may hold "\n" for hard line breaks
}

var (
	reImage    = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	reLink     = regexp.MustCompile(`\[([^\]]*)\]\((?:[^()]|\([^)]*\))*\)`)
	reAuto     = regexp.MustCompile(`<(https?://[^>]+)>`)
	reEmph     = regexp.MustCompile(`\*\*([^*]+)\*\*|__([^_]+)__`)
	reEscape   = regexp.MustCompile(`\\([\\*_\[\]()#>|!.+-])`)
	reSpaces   = regexp.MustCompile(`[ \t\x{00a0}]+`)
	reTableSep = regexp.MustCompile(`^\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?$`)
	reBullet   = regexp.MustCompile(`^([-*+]|\d+[.)])\s+`)
	reRule     = regexp.MustCompile(`^(\*\s*){3,}$|^(-\s*){3,}$|^(_\s*){3,}$`)
)

// invisible are characters marketing mail pads its preview text with.
var invisible = strings.NewReplacer("\u200b", "", "\u200c", "", "\u200d", "", "\u200e", "", "\u200f", "", "\u034f", "", "\u00ad", "", "\ufeff", "", "\u2060", "", "\u2061", "", "\u2062", "", "\u2063", "", "\u2064", "")

// inline turns one line of Markdown into plain text: images go, links keep
// their text, emphasis markers and escapes go, entities decode.
func inline(s string) string {
	s = reImage.ReplaceAllString(s, "")
	s = reLink.ReplaceAllStringFunc(s, func(m string) string {
		text := reLink.FindStringSubmatch(m)[1]
		if t := strings.ToLower(strings.TrimSpace(text)); t == "image" || t == "attachment" {
			return ""
		}
		return text
	})
	s = reAuto.ReplaceAllString(s, "$1")
	s = reEmph.ReplaceAllString(s, "$1$2")
	s = reEscape.ReplaceAllString(s, "$1")
	s = html.UnescapeString(s)
	s = invisible.Replace(s)
	return strings.TrimSpace(reSpaces.ReplaceAllString(s, " "))
}

// cleanBody turns a message's Markdown into paragraphs to draw.
func cleanBody(md string) []para {
	var out []para
	var cur []string
	curKind := paraText
	hard := false
	flush := func() {
		if len(cur) > 0 {
			out = append(out, para{curKind, strings.Join(cur, "")})
		}
		cur, curKind, hard = nil, paraText, false
	}
	for _, raw := range strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n") {
		breakAfter := strings.HasSuffix(raw, "  ") || strings.HasSuffix(raw, "\\")
		s := strings.TrimSpace(strings.TrimSuffix(raw, "\\"))
		kind := paraText
		for strings.HasPrefix(s, ">") {
			kind = paraQuote
			s = strings.TrimSpace(s[1:])
		}
		if reTableSep.MatchString(s) && strings.Contains(s, "-") && strings.Contains(s, "|") || reRule.MatchString(s) {
			flush()
			continue
		}
		if strings.HasPrefix(s, "|") {
			var cells []string
			for _, c := range strings.Split(strings.Trim(s, "|"), "|") {
				if c = inline(c); c != "" {
					cells = append(cells, c)
				}
			}
			s = strings.Join(cells, "  ·  ")
			flush()
			if s != "" {
				out = append(out, para{kind, s})
			}
			continue
		}
		if kind != paraQuote && strings.HasPrefix(s, "#") {
			flush()
			if h := inline(strings.TrimLeft(s, "# ")); h != "" {
				out = append(out, para{paraHeading, h})
			}
			continue
		}
		bullet := false
		if m := reBullet.FindString(s); m != "" && kind != paraQuote {
			bullet = true
			s = s[len(m):]
		}
		s = inline(s)
		if s == "" {
			flush()
			continue
		}
		if bullet {
			flush()
			out = append(out, para{paraBullet, s})
			continue
		}
		if len(cur) > 0 && kind != curKind {
			flush()
		}
		if len(cur) > 0 {
			if hard {
				cur = append(cur, "\n")
			} else {
				cur = append(cur, " ")
			}
		}
		cur = append(cur, s)
		curKind, hard = kind, breakAfter
	}
	flush()
	return out
}

// threadText is a thread as plain text, for handing to an agent.
func threadText(p Posting, entries []Entry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Subject: %s\n", p.Subject)
	fmt.Fprintf(&b, "HEY thread ID: %d\nLink: %s\n\n", p.TopicID, p.AppURL)
	for _, e := range entries {
		fmt.Fprintf(&b, "---\nFrom: %s <%s>\nDate: %s\n\n", e.sender(), e.Creator.Email, e.when().Local().Format("Mon Jan 2, 2006 3:04 PM"))
		for _, pr := range cleanBody(firstNonEmpty(e.Body, e.Summary)) {
			switch pr.kind {
			case paraQuote:
				for _, ln := range strings.Split(pr.text, "\n") {
					b.WriteString("> " + ln + "\n")
				}
			case paraBullet:
				b.WriteString("- " + pr.text + "\n")
			default:
				b.WriteString(pr.text + "\n")
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// shortDate is how the list shows when a thread was last active.
func shortDate(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	y1, m1, d1 := t.Date()
	y2, m2, d2 := now.Date()
	switch {
	case y1 == y2 && m1 == m2 && d1 == d2:
		return t.Format("3:04 PM")
	case now.Sub(t) < 6*24*time.Hour && t.Before(now):
		return t.Format("Mon")
	case y1 == y2:
		return t.Format("Jan 2")
	}
	return t.Format("Jan 2, 2006")
}
