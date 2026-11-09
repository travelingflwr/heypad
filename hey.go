package main

// The data layer: everything heypad knows about mail comes from the hey CLI's
// JSON output. Results are cached on disk so the viewer opens on the last
// known state at once and refreshes behind it.

import (
	"bytes"
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
	// Seen is HEY's: true once the thread is in Previously Seen. HEY leaves
	// it out for New For You.
	Seen bool `json:"seen"`
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

// boxPage is one fetch of a box: its threads.
type boxPage struct {
	posts []Posting
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
		} `json:"data"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		return boxPage{}, fmt.Errorf("reading hey's reply: %w", err)
	}
	if !env.OK {
		return boxPage{}, errors.New(firstNonEmpty(env.Error, "hey reported an error"))
	}
	return boxPage{env.Data.Postings}, nil
}

// newCount is how many of the Imbox's first threads are in New For You.
// The Imbox lists New For You, then Previously Seen, and HEY flags each
// posting in Previously Seen as seen, so New For You is the run of
// unflagged threads at the top. (Ordering alone can't find the boundary:
// when everything new is newer than everything seen, the two sections read
// as one newest-first run.) If the whole page is unflagged, there may be
// more New For You past it.
func newCount(posts []Posting) int {
	for i, p := range posts {
		if p.Seen {
			return i
		}
	}
	return len(posts)
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
		n := newCount(pg.posts)
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

// markSeenIDs marks many box items seen, a batch per call.
func markSeenIDs(ids []int64) error {
	for len(ids) > 0 {
		n := min(50, len(ids))
		args := []string{"seen"}
		for _, id := range ids[:n] {
			args = append(args, fmt.Sprint(id))
		}
		if _, err := heyJSON(args...); err != nil {
			return err
		}
		ids = ids[n:]
	}
	return nil
}

// --- triage ---------------------------------------------------------------

// Triage is what an agent made of New For You, written to triageFile while
// it works; heypad shows each verdict on the thread's row.
type Triage struct {
	Threads map[string]TriageItem `json:"threads"` // by box item ID
}

type TriageItem struct {
	Label string `json:"label"` // needs_you, read, or skip
	Why   string `json:"why"`
}

const (
	triageFile    = "triage.json"
	triageNewFile = "new-for-you.json"
)

func triagePath() string { return filepath.Join(stateDir(), triageFile) }

// loadTriage reads the triage file; a missing or half-written one is empty.
func loadTriage() map[int64]TriageItem {
	var t Triage
	out := map[int64]TriageItem{}
	if !readCache(triageFile, &t) {
		return out
	}
	for k, v := range t.Threads {
		var id int64
		if _, err := fmt.Sscan(k, &id); err == nil {
			switch v.Label {
			case "needs_you", "read", "skip":
				out[id] = v
			}
		}
	}
	return out
}

// triageInput is New For You as the agent gets it: enough to sort by,
// with the IDs it needs to look closer or mark seen.
type triageInput struct {
	BoxItemID int64     `json:"box_item_id"`
	ThreadID  int64     `json:"thread_id"`
	From      string    `json:"from"`
	Email     string    `json:"email"`
	Subject   string    `json:"subject"`
	Preview   string    `json:"preview"`
	When      time.Time `json:"when"`
	Messages  int       `json:"messages"`
}

func writeTriageInput(posts []Posting) (string, error) {
	var in []triageInput
	for _, p := range posts {
		if p.New {
			in = append(in, triageInput{p.ID, p.TopicID, p.sender(), p.Creator.Email, p.Subject, inline(p.Summary), p.ActiveAt, p.Count})
		}
	}
	data, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return "", err
	}
	os.MkdirAll(stateDir(), 0o700)
	path := filepath.Join(stateDir(), triageNewFile)
	return path, os.WriteFile(path, data, 0o600)
}
