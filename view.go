package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"image"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"slices"
	"syscall"
	"time"

	"golang.org/x/term"
)

type viewOptions struct {
	theme    string  // terminal (follow the terminal's colors), light, or dark
	scale    float64 // device pixels per point; 0 picks from the cell size
	floating bool    // running in heypad's floating window
}

const (
	enterScreen = "\x1b[?1049h\x1b[?25l\x1b[H\x1b[2J\x1b[?2031h\x1b[?1000h\x1b[?1006h\x1b[?1016h\x1b[?2004h\x1b[>1u"
	leaveScreen = "\x1b_Ga=d,d=A,q=2\x1b\\\x1b[<u\x1b[?2004l\x1b[?1016l\x1b[?1006l\x1b[?1000l\x1b[?2031l\x1b[?25h\x1b[?1049l"

	animFrame    = 8 * time.Millisecond
	easeTau      = 45 * time.Millisecond
	colorPoll    = 250 * time.Millisecond
	colorSettle  = 30 * time.Millisecond
	refreshEvery = 90 * time.Second
	maxSheetPx   = 8192
	lineStep     = 40.0 // points a line scroll moves the reader
	introPt      = 22.0 // how far a thread rises as it opens
)

type screenKind int

const (
	screenList screenKind = iota
	screenThread
)

type threadState struct {
	post    Posting
	entries []Entry
	loading bool
	err     string
	ver     int
}

type resultKind int

const (
	resBox resultKind = iota
	resThread
	resWrite
	resNote
	resSeen    // a bulk mark-seen finished
	resRefresh // something outside (the triage agent) may have changed the box
	resImage   // an image in a message finished loading
)

type result struct {
	kind    resultKind
	box     string
	posts   []Posting
	topic   int64
	entries []Entry
	err     error
	ok      bool
	msg     string
	ids     []int64
}

// geom is the pane's size and how it's divided: a header of whole rows at
// the top, the content below.
type geom struct {
	ws                     winSize
	cellW, cellH           int
	scale                  float64
	hdrRows, rows          int
	widthPx, hdrPx, viewPx int
}

func (g geom) widthPt() float64 { return float64(g.widthPx) / g.scale }
func (g geom) viewH() float64   { return float64(g.viewPx) / g.scale }

func measure(fd int, scaleFlag float64) geom {
	ws := getWinSize(fd)
	g := geom{ws: ws, cellW: ws.xpix / ws.cols, cellH: ws.ypix / ws.rows}
	g.scale = pickScale(scaleFlag, ws)
	g.hdrRows = max(1, min(ws.rows-1, int(math.Round(headerPt*g.scale/float64(g.cellH)))))
	g.rows = max(1, ws.rows-g.hdrRows)
	g.widthPx, g.hdrPx, g.viewPx = g.cellW*ws.cols, g.cellH*g.hdrRows, g.cellH*g.rows
	return g
}

func easeToward(shown, target float64, dt time.Duration, scale float64) float64 {
	k := 1 - math.Exp(-float64(dt)/float64(easeTau))
	shown += (target - shown) * k
	if math.Abs(target-shown)*scale < 0.5 {
		return target
	}
	return shown
}

func openURL(url string) {
	if url == "" {
		return
	}
	cmd := "xdg-open"
	if runtime.GOOS == "darwin" {
		cmd = "open"
	}
	exec.Command(cmd, url).Start()
}

func runView(opt viewOptions) error {
	fonts, err := loadFonts()
	if err != nil {
		return err
	}
	in, out := int(os.Stdin.Fd()), os.Stdout
	var saved *term.State
	if term.IsTerminal(in) {
		if saved, err = term.MakeRaw(in); err == nil {
			defer func() { term.Restore(in, saved) }()
		}
	}
	events := make(chan termEvent, 256)
	reader := &ttyReader{}
	go reader.run(events)

	w := bufio.NewWriterSize(out, 1<<16)
	fmt.Fprint(w, enterScreen)
	w.Flush()
	defer func() {
		fmt.Fprint(w, leaveScreen)
		w.Flush()
	}()

	// Wait for the theme so the first frame has the right colors.
	var colors termColors
	queryColors(w)
	w.Flush()
	if opt.theme == "terminal" {
		deadline := time.After(3 * time.Second)
		retry := time.NewTicker(250 * time.Millisecond)
	wait:
		for !(colors.hasFg && colors.hasBg && colors.hasBlue) {
			select {
			case ev, open := <-events:
				if !open {
					retry.Stop()
					return nil
				}
				colors.apply(ev.osc)
			case <-retry.C:
				queryColors(w)
				w.Flush()
			case <-deadline:
				break wait
			}
		}
		retry.Stop()
	}
	pal := colors.palette(opt.theme)
	clear := opt.theme == "terminal"
	drawPal := func() palette {
		if clear {
			return pal.seeThrough()
		}
		return pal
	}

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	// kill -USR1 saves what's on screen, for debugging (heypad snapshot).
	snap := make(chan os.Signal, 1)
	signal.Notify(snap, syscall.SIGUSR1)
	results := make(chan result, 32)
	imgs := newImageCache(func(src string) { go func() { results <- result{kind: resImage, msg: src} }() })
	// Images finishing together lay the thread out once, not once each.
	var imgSettle <-chan time.Time

	// --- state ----------------------------------------------------------

	var (
		boxIdx  int
		posts   = map[string][]Posting{}
		loading = map[string]bool{}
		errs    = map[string]string{}
		vers    = map[string]int{}
		sels    = map[string]int{}
		listOff = map[string]float64{}

		screen screenKind
		th     *threadState

		status      string
		statusRole  role
		statusUntil time.Time

		shown, target     float64 // the content's scroll offset, in points
		hlShown, hlTarget float64 // the selection's row top, in points
		started           bool

		anim     *time.Ticker
		animC    <-chan time.Time
		lastStep time.Time

		nextID = 1
		// What's on screen: each image's ID and what it was drawn from.
		hdrID, sheetID, hlID, bgID     int
		hdrKey, sheetKey, hlKey, bgKey string
		placedKey                      string
		sheetTop, sheetH               int
		lastGeom                       geom

		docKey string
		docv   *doc

		sheetImg *image.RGBA // the sheet's pixels, to cut the faded edges from
		bandIDs  [2]int      // the faded top and bottom edges, 0 for none

		// The floating window shows only what's new; a full window shows
		// the Imbox's two sections. s switches.
		showAll = !opt.floating
		// A thread opened from New For You, to move out of it on the way
		// back to the list (HEY already has it as seen).
		pendingRead int64

		// Bulk mark seen: the marked threads (by box item ID), and the ones
		// waiting on a yes while confirming.
		marked      = map[int64]bool{}
		confirmSeen []int64
		markIDs     []int // the check images over marked avatars
		markKey     string

		// Triage: an agent's verdicts, reloaded when its file changes.
		triage      = loadTriage()
		triageVer   int
		triageStamp string

		// For debug snapshots: the last header and selection images, and
		// the view row the selection's image starts on.
		hdrImg, hlImg *image.RGBA
		hlRow         int
	)
	for _, b := range boxes {
		posts[b.kind] = cachedBox(b.kind)
	}
	kind := func() string { return boxes[boxIdx].kind }
	onlyNew := func(k string) bool { return k == "imbox" && !showAll }
	visible := func(k string) []Posting {
		if !onlyNew(k) {
			return posts[k]
		}
		var out []Posting
		for _, p := range posts[k] {
			if p.New {
				out = append(out, p)
			}
		}
		return out
	}
	cur := func() []Posting { return visible(kind()) }
	lay := func() listLayout { return layoutList(cur(), kind() == "imbox" && showAll) }
	top := func(i int) float64 {
		if l := lay(); i >= 0 && i < len(l.tops) {
			return l.tops[i]
		}
		return listTop
	}
	newIn := func(k string) int {
		n := 0
		for _, p := range posts[k] {
			if p.New {
				n++
			}
		}
		return n
	}
	// indexOf finds a thread in the visible list by its box item ID.
	indexOf := func(id int64) int {
		for i, p := range cur() {
			if p.ID == id {
				return i
			}
		}
		return -1
	}
	// readNow moves a thread out of New For You, to the top of Previously
	// Seen, the way HEY files it once it's seen.
	readNow := func(k string, id int64) {
		list := posts[k]
		i := slices.IndexFunc(list, func(p Posting) bool { return p.ID == id })
		if i < 0 || !list[i].New {
			return
		}
		p := list[i]
		p.New = false
		list = slices.Delete(slices.Clone(list), i, i+1)
		at := 0
		for at < len(list) && list[at].New {
			at++
		}
		posts[k] = slices.Insert(list, at, p)
		vers[k]++
	}
	sel := func() int {
		n := len(cur())
		s := sels[kind()]
		if s >= n {
			s = n - 1
		}
		return max(0, s)
	}
	selected := func() (Posting, bool) {
		if p := cur(); len(p) > 0 {
			return p[sel()], true
		}
		return Posting{}, false
	}
	setStatus := func(msg string, ro role) {
		status, statusRole, statusUntil = msg, ro, time.Now().Add(5*time.Second)
	}
	setTitle := func() {
		t := "HEY · " + boxes[boxIdx].title
		if n := newIn(kind()); n > 0 {
			t += fmt.Sprintf(" (%d)", n)
		}
		if screen == screenThread && th != nil {
			t = "HEY · " + th.post.Subject
		}
		fmt.Fprintf(w, "\x1b]2;%s\x07", t)
	}

	fetchBoxAsync := func(k string) {
		if loading[k] {
			return
		}
		loading[k] = true
		go func() {
			p, err := fetchBox(k, newIn(k))
			results <- result{kind: resBox, box: k, posts: p, err: err}
		}()
	}
	fetchThreadAsync := func(topic int64) {
		go func() {
			e, err := fetchThread(topic)
			results <- result{kind: resThread, topic: topic, entries: e, err: err}
		}()
	}

	startAnim := func() {
		if anim == nil {
			anim = time.NewTicker(animFrame)
			animC = anim.C
			lastStep = time.Now().Add(-animFrame)
		}
	}
	stopAnim := func() {
		if anim != nil {
			anim.Stop()
			anim, animC = nil, nil
		}
	}

	threadDoc := func(r *renderer, g geom) *doc {
		note := ""
		if th.err != "" {
			note = "Couldn't load this thread: " + th.err
		} else if th.loading && len(th.entries) == 0 {
			note = "Loading…"
		} else if t, ok := triage[th.post.ID]; ok && t.Why != "" {
			note = "Claude: " + triageLabels[t.Label] + ". " + t.Why
		}
		entries := th.entries
		if len(entries) == 0 {
			entries = []Entry{{Creator: th.post.Creator, AltSender: th.post.AltSender, Summary: th.post.Summary,
				CreatedAt: th.post.ActiveAt.UTC().Format("2006-01-02T15:04")}}
		}
		key := fmt.Sprint(th.post.TopicID, th.ver, note, g.widthPx, g.scale)
		if key != docKey || docv == nil {
			docKey, docv = key, r.layoutThread(th.post, entries, g.widthPt(), note)
		}
		return docv
	}
	contentHeight := func(r *renderer, g geom) float64 {
		if screen == screenThread && th != nil {
			return threadDoc(r, g).height
		}
		return lay().height
	}
	maxOffset := func(r *renderer, g geom) float64 {
		return math.Max(0, contentHeight(r, g)-g.viewH())
	}
	newRenderer := func(g geom) *renderer {
		return &renderer{fonts: fonts, scale: g.scale, pal: drawPal(), clear: clear, now: time.Now(), triage: triage, imgs: imgs}
	}

	// keepSelVisible scrolls the list so the selected row shows with some
	// room around it.
	keepSelVisible := func() {
		g := measure(int(out.Fd()), opt.scale)
		r := newRenderer(g)
		top := top(sel())
		if sel() == 0 {
			top = 0 // the first row brings its section heading into view
		}
		margin := math.Min(rowPitch/2, math.Max(0, (g.viewH()-rowPitch)/2))
		if top-margin < target {
			target = top - margin
		}
		if top+rowPitch+margin > target+g.viewH() {
			target = top + rowPitch + margin - g.viewH()
		}
		target = math.Max(0, math.Min(target, maxOffset(r, g)))
		// An empty list (all caught up) has no rows to highlight.
		hlTarget = listTop
		if l := lay(); sel() < len(l.tops) {
			hlTarget = l.tops[sel()]
		}
		if shown != target || hlShown != hlTarget {
			startAnim()
		}
	}

	headerFor := func() header {
		h := header{}
		if screen == screenThread && th != nil {
			h.left, h.leftRole = "‹ "+boxes[boxIdx].title, roleAccent
			h.right, h.rightRole = "r reply   a ask Claude   O open", roleFaint
		} else {
			h.left, h.leftBold = boxes[boxIdx].title, true
			if n := newIn(kind()); n > 0 {
				h.badge = fmt.Sprintf("%d new", n)
			}
			h.rightRole = roleFaint
			switch {
			case errs[kind()] != "":
				h.right, h.rightRole = errs[kind()], roleGray
			case loading[kind()]:
				h.right = "Updating…"
			case opt.floating && kind() == "imbox" && showAll:
				h.right = "s new only   c compose"
			case opt.floating && kind() == "imbox":
				h.right = "s all mail   c compose"
			default:
				h.right = "Tab next box   c compose"
			}
		}
		if screen == screenList {
			switch {
			case confirmSeen != nil:
				h.right, h.rightRole = fmt.Sprintf("Mark %d seen?   y yes   n no", len(confirmSeen)), roleAccent
			case len(marked) > 0:
				h.right, h.rightRole = fmt.Sprintf("%d marked   e mark seen   Esc clear", len(marked)), roleAccent
			}
		}
		if status != "" && time.Now().Before(statusUntil) && confirmSeen == nil {
			h.right, h.rightRole = status, statusRole
		}
		return h
	}

	// --- drawing --------------------------------------------------------

	drawFrame := func(force bool) {
		g := measure(int(out.Fd()), opt.scale)
		if g != lastGeom {
			force = true
			lastGeom = g
		}
		r := newRenderer(g)
		maxOff := maxOffset(r, g)
		target = math.Max(0, math.Min(target, maxOff))
		if !started {
			shown, hlShown, started = target, hlTarget, true
		}
		shown = math.Max(-introPt, math.Min(shown, maxOff))
		var drop []int
		fmt.Fprint(w, "\x1b[?2026h")

		// A fixed theme paints its own background, under everything.
		if !clear {
			key := fmt.Sprint(pal.bg, g)
			if force || key != bgKey {
				img := image.NewRGBA(image.Rect(0, 0, 8, 8))
				(&renderer{pal: pal}).paint(img)
				id := nextID
				nextID++
				kittyUpload(w, img, id)
				kittyPlace(w, id, 1, 1, 0, 8, 8, g.ws.cols, g.ws.rows, -3)
				drop = append(drop, bgID)
				bgID, bgKey = id, key
			}
		}

		// The header.
		h := headerFor()
		key := fmt.Sprint(h, pal, g)
		if force || key != hdrKey {
			img := image.NewRGBA(image.Rect(0, 0, g.widthPx, g.hdrPx))
			r.drawHeader(img, h)
			id := nextID
			nextID++
			kittyUpload(w, img, id)
			kittyPlace(w, id, 1, 1, 0, g.widthPx, g.hdrPx, g.ws.cols, g.hdrRows, 0)
			drop = append(drop, hdrID)
			hdrID, hdrKey, hdrImg = id, key, img
		}

		// The content: one tall sheet, uploaded once and scrolled by moving
		// the window onto it that's placed.
		offsetPx := int(math.Round(shown * g.scale))
		var ck string
		if screen == screenThread && th != nil {
			threadDoc(r, g)
			ck = fmt.Sprint("t", docKey, pal)
		} else {
			ck = fmt.Sprint("l", kind(), vers[kind()], showAll, triageVer, loading[kind()] && len(cur()) == 0, errs[kind()], pal, g, time.Now().Format("2006-01-02 15:04"))
		}
		if force || ck != sheetKey || offsetPx < sheetTop || offsetPx+g.viewPx > sheetTop+sheetH {
			// The sheet can start above the content (a negative offset) while
			// a thread rises into place.
			contentPx := int(math.Ceil(contentHeight(r, g) * g.scale))
			lo := min(0, offsetPx)
			height := max(g.viewPx, min(contentPx-lo, maxSheetPx))
			top := lo
			if contentPx-lo > height {
				top = max(lo, min(offsetPx-(height-g.viewPx)/2, contentPx-height))
			}
			img := image.NewRGBA(image.Rect(0, 0, g.widthPx, height))
			if screen == screenThread && th != nil {
				r.drawDoc(img, docv, float64(top)/g.scale)
			} else {
				msg := ""
				switch {
				case errs[kind()] != "":
					msg = "Couldn't load " + boxes[boxIdx].title + ": " + errs[kind()]
				case loading[kind()]:
					msg = "Loading " + boxes[boxIdx].title + "…"
				default:
					msg = "Nothing in " + boxes[boxIdx].title + "."
				}
				done := onlyNew(kind()) && len(cur()) == 0 && errs[kind()] == "" && !(loading[kind()] && len(posts[kind()]) == 0)
				if done {
					msg = "All caught up"
				}
				r.drawList(img, cur(), lay(), float64(top)/g.scale, msg, done)
			}
			id := nextID
			nextID++
			kittyUpload(w, img, id)
			drop = append(drop, sheetID)
			sheetID, sheetKey, sheetTop, sheetH, sheetImg = id, ck, top, height, img
		}

		// Where there's more to scroll to, that edge fades out: the edges are
		// small images cut from the sheet and faded, and the sheet's crop is
		// placed between them.
		sy := offsetPx - sheetTop
		fr, fadePx := 0, 0
		fadeTop, fadeBot := 0.0, 0.0
		if maxOff > 0 {
			fr = fadeRowsFor(g.scale, g.cellH, g.rows)
			fadePx = fr * g.cellH
			if fr > 0 {
				fadeTop, fadeBot = edgeFades(max(0, offsetPx), int(math.Round(maxOff*g.scale)), fadePx)
			}
		}
		if fadeTop == 0 && fadeBot == 0 {
			fr, fadePx = 0, 0
		}
		pk := fmt.Sprint(sheetID, sy, fr, fadeTop, fadeBot, g)
		if force || pk != placedKey {
			if fr > 0 {
				kittyPlace(w, sheetID, g.hdrRows+1+fr, 1, sy+fadePx, g.widthPx, g.viewPx-2*fadePx, g.ws.cols, g.rows-2*fr, 0)
				var next [2]int
				for i, band := range []*image.RGBA{
					fadeBand(sheetImg, sy, fadePx, true, fadeTop, clear, pal.bg),
					fadeBand(sheetImg, sy+g.viewPx-fadePx, fadePx, false, fadeBot, clear, pal.bg),
				} {
					next[i] = nextID
					nextID++
					kittyUpload(w, band, next[i])
				}
				kittyPlace(w, next[0], g.hdrRows+1, 1, 0, g.widthPx, fadePx, g.ws.cols, fr, 0)
				kittyPlace(w, next[1], g.hdrRows+g.rows-fr+1, 1, 0, g.widthPx, fadePx, g.ws.cols, fr, 0)
				drop = append(drop, bandIDs[:]...)
				bandIDs = next
			} else {
				kittyPlace(w, sheetID, g.hdrRows+1, 1, sy, g.widthPx, g.viewPx, g.ws.cols, g.rows, 0)
				drop = append(drop, bandIDs[:]...)
				bandIDs = [2]int{}
			}
			placedKey = pk
		}

		// The selection: a small image under the sheet, cut to whole rows,
		// so moving it never redraws the list.
		hlWanted := false
		if screen == screenList && len(cur()) > 0 {
			y0 := hlShown*g.scale - float64(offsetPx) // the row's top, in view pixels
			top := math.Max(0, y0+3*g.scale-2)
			bot := math.Min(float64(g.viewPx), y0+(rowPitch-3)*g.scale+2)
			if bot > top {
				hlWanted = true
				r0, r1 := int(top)/g.cellH, min(g.rows, int(math.Ceil(bot/float64(g.cellH))))
				key := fmt.Sprint(math.Round(y0), r0, r1, pal, g, fr, fadeTop, fadeBot)
				if force || key != hlKey || hlID == 0 {
					img := image.NewRGBA(image.Rect(0, 0, g.widthPx, (r1-r0)*g.cellH))
					r.selection(img, (y0-float64(r0*g.cellH))/g.scale)
					if fr > 0 { // fade it with the text it's under
						fadeRows(img, -r0*g.cellH, fadePx, true, fadeTop, true, pal.bg)
						fadeRows(img, g.viewPx-fadePx-r0*g.cellH, fadePx, false, fadeBot, true, pal.bg)
					}
					id := nextID
					nextID++
					kittyUpload(w, img, id)
					kittyPlace(w, id, g.hdrRows+1+r0, 1, 0, g.widthPx, (r1-r0)*g.cellH, g.ws.cols, r1-r0, -1)
					drop = append(drop, hlID)
					hlID, hlKey, hlImg, hlRow = id, key, img, r0
				}
			}
		}
		if !hlWanted && hlID != 0 {
			drop = append(drop, hlID)
			hlID, hlKey = 0, ""
		}

		// Marked threads: a check over each avatar in view, each a small
		// image over the sheet, so marking never redraws the list.
		type spot struct {
			r0, r1, c1 int
			cy         float64 // the avatar's center, in the image, in points
		}
		var spots []spot
		if screen == screenList && len(marked) > 0 {
			l := lay()
			c1 := min(g.ws.cols, int(math.Ceil((padX+2*avatarR+2)*g.scale/float64(g.cellW))))
			for i, p := range cur() {
				if !marked[p.ID] {
					continue
				}
				cy := (l.tops[i]+30)*g.scale - float64(offsetPx) // view pixels
				top, bot := cy-(avatarR+1)*g.scale, cy+(avatarR+1)*g.scale
				if bot <= 0 || top >= float64(g.viewPx) {
					continue
				}
				r0, r1 := max(0, int(top)/g.cellH), min(g.rows, int(math.Ceil(bot/float64(g.cellH))))
				if r1 > r0 {
					spots = append(spots, spot{r0, r1, c1, (cy - float64(r0*g.cellH)) / g.scale})
				}
			}
		}
		if key := fmt.Sprint(spots, pal, g); force || key != markKey {
			var ids []int
			for _, sp := range spots {
				img := image.NewRGBA(image.Rect(0, 0, sp.c1*g.cellW, (sp.r1-sp.r0)*g.cellH))
				r.markBadge(img, padX+avatarR, sp.cy)
				id := nextID
				nextID++
				kittyUpload(w, img, id)
				kittyPlace(w, id, g.hdrRows+1+sp.r0, 1, 0, sp.c1*g.cellW, (sp.r1-sp.r0)*g.cellH, sp.c1, sp.r1-sp.r0, 1)
				ids = append(ids, id)
			}
			drop = append(drop, markIDs...)
			markIDs, markKey = ids, key
		}

		for _, id := range drop {
			if id != 0 {
				kittyDelete(w, id)
			}
		}
		fmt.Fprint(w, "\x1b[?2026l")
	}
	draw := func(force bool) {
		drawFrame(force)
		w.Flush()
	}
	step := func(now time.Time) {
		dt := now.Sub(lastStep)
		lastStep = now
		g := lastGeom
		shown = easeToward(shown, target, dt, g.scale)
		hlShown = easeToward(hlShown, hlTarget, dt, g.scale)
		if shown == target && hlShown == hlTarget {
			stopAnim()
		}
		draw(false)
	}

	// --- actions --------------------------------------------------------

	snapList := func() {
		hlTarget = top(sel())
		hlShown = hlTarget
		target = listOff[kind()]
		shown = target
	}
	moveSel := func(d int) {
		n := len(cur())
		if n == 0 {
			return
		}
		sels[kind()] = max(0, min(n-1, sel()+d))
		keepSelVisible()
		step(time.Now())
	}
	switchBox := func(i int) {
		listOff[kind()] = target
		boxIdx = (i + len(boxes)) % len(boxes)
		snapList()
		setTitle()
		fetchBoxAsync(kind())
		draw(false)
	}
	openThread := func(p Posting) {
		if screen == screenList {
			listOff[kind()] = target
		}
		screen = screenThread
		th = &threadState{post: p, entries: cachedThread(p.TopicID), loading: true}
		if p.New {
			pendingRead = p.ID
		}
		// The reader rises the last few points into place.
		shown, target = -introPt, 0
		startAnim()
		setTitle()
		fetchThreadAsync(p.TopicID)
		go markSeen(p)
		draw(false)
	}
	// settleRead files the thread just read under Previously Seen. In the
	// floating window it leaves the list, and the selection lands on the
	// next new thread.
	settleRead := func() {
		if pendingRead != 0 {
			readNow(kind(), pendingRead)
			pendingRead = 0
		}
	}
	back := func() {
		screen, th = screenList, nil
		settleRead()
		snapList()
		setTitle()
		draw(false)
	}
	stepThread := func(d int) {
		p := cur()
		i := sel() + d
		if i < 0 || i >= len(p) {
			setStatus("No more threads that way", roleFaint)
			draw(false)
			return
		}
		next := p[i]
		settleRead()
		sels[kind()] = max(0, indexOf(next.ID))
		openThread(next)
	}
	scrollBy := func(dy float64) {
		g := lastGeom
		target = math.Max(0, math.Min(target+dy, maxOffset(newRenderer(g), g)))
		if shown != target {
			idle := anim == nil
			startAnim()
			if idle {
				step(time.Now())
			}
		}
	}

	// suspend hands the terminal to run (an editor) outside of Rex, where
	// there are no floating panes to open it in.
	suspend := func(run func()) {
		reader.pause()
		fmt.Fprint(w, leaveScreen)
		w.Flush()
		if saved != nil {
			term.Restore(in, saved)
		}
		run()
		if saved != nil {
			saved, _ = term.MakeRaw(in)
		}
		fmt.Fprint(w, enterScreen)
		hdrID, sheetID, hlID, bgID = 0, 0, 0, 0
		reader.resume()
		setTitle()
		draw(true)
	}
	write := func(p *Posting) {
		if !inRex() {
			self, _ := os.Executable()
			args := []string{"write"}
			if p != nil {
				args = append(args, "--reply", fmt.Sprint(p.TopicID), "--subject", p.Subject, "--to", p.sender())
			}
			suspend(func() {
				cmd := exec.Command(self, args...)
				cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
				cmd.Run()
			})
			if p != nil {
				fetchThreadAsync(p.TopicID)
			}
			return
		}
		block, token, err := openWriter(p)
		if err != nil {
			setStatus(err.Error(), roleGray)
			draw(false)
			return
		}
		var topic int64
		if p != nil {
			topic = p.TopicID
		}
		go func() {
			waitBlock(block)
			res := writeResult{Message: "Cancelled"}
			if data, err := os.ReadFile(resultPath(token)); err == nil {
				json.Unmarshal(data, &res)
				os.Remove(resultPath(token))
			}
			results <- result{kind: resWrite, topic: topic, ok: res.OK, msg: res.Message}
		}()
	}
	ask := func(p Posting) {
		if !inRex() {
			setStatus("Ask Claude opens in a Rex floating pane", roleGray)
			draw(false)
			return
		}
		entries := cachedThread(p.TopicID)
		if th != nil && th.post.TopicID == p.TopicID && len(th.entries) > 0 {
			entries = th.entries
		}
		setStatus("Opening Claude…", roleFaint)
		draw(false)
		go func() {
			if len(entries) == 0 {
				var err error
				if entries, err = fetchThread(p.TopicID); err != nil {
					results <- result{kind: resNote, msg: "Couldn't load the thread: " + err.Error()}
					return
				}
			}
			if _, err := askAgent(p, entries); err != nil {
				results <- result{kind: resNote, msg: err.Error()}
			}
		}()
	}

	// markedIDs are the marked threads still in the box, in list order.
	markedIDs := func() []int64 {
		var ids []int64
		for _, p := range posts[kind()] {
			if marked[p.ID] {
				ids = append(ids, p.ID)
			}
		}
		return ids
	}
	confirmed := func() {
		ids := confirmSeen
		confirmSeen = nil
		setStatus(fmt.Sprintf("Marking %d seen…", len(ids)), roleFaint)
		draw(false)
		go func() {
			results <- result{kind: resSeen, box: kind(), ids: ids, err: markSeenIDs(ids)}
		}()
	}
	triageNow := func() {
		if !inRex() {
			setStatus("Triage opens in a Rex floating pane", roleGray)
			draw(false)
			return
		}
		if newIn("imbox") == 0 {
			setStatus("Nothing new to triage", roleFaint)
			draw(false)
			return
		}
		block, err := triageAgent(posts["imbox"])
		if err != nil {
			setStatus(err.Error(), roleGray)
			draw(false)
			return
		}
		triage, triageStamp = map[int64]TriageItem{}, ""
		triageVer++
		setStatus("Claude is triaging New For You…", roleAccent)
		draw(false)
		go func() {
			waitBlock(block)
			results <- result{kind: resRefresh, box: "imbox"}
		}()
	}

	handleKey := func(k keyPress) bool {
		g := lastGeom
		page := math.Max(rowPitch, g.viewH()-rowPitch)
		if k.is('c', modCtrl) || k.is('d', modCtrl) {
			return true
		}
		// The toggle shortcut, when a Rex client hands it to the pane instead
		// of running its binding (the iPad app does, for now): hide.
		if k.is('m', modSuper|modShift) || k.is('M', modSuper|modShift) || k.is('M', modSuper) {
			if opt.floating {
				minimizeSelf()
			}
			return false
		}
		if confirmSeen != nil {
			switch {
			case k.is('y', 0) || k.is(keyEnter, 0):
				confirmed()
			case k.is('n', 0) || k.is(keyEscape, 0) || k.is('q', 0):
				confirmSeen = nil
				draw(false)
			}
			return false
		}
		if screen == screenThread && th != nil {
			switch {
			case k.is(keyEscape, 0) || k.is('h', 0) || k.is(keyLeft, 0) || k.is(keyBackspace, 0) || k.is('q', 0):
				back()
			case k.is('j', 0) || k.is(keyDown, 0):
				scrollBy(lineStep)
			case k.is('k', 0) || k.is(keyUp, 0):
				scrollBy(-lineStep)
			case k.is(' ', 0) || k.is('d', 0) || k.is(keyPageDown, 0):
				scrollBy(page)
			case k.is('b', 0) || k.is('u', 0) || k.is(keyPageUp, 0):
				scrollBy(-page)
			case k.is('g', 0) || k.is(keyHome, 0):
				scrollBy(-math.MaxFloat32)
			case k.is('G', 0) || k.is('g', modShift) || k.is(keyEnd, 0):
				scrollBy(math.MaxFloat32)
			case k.is('n', 0) || k.is('J', 0):
				stepThread(1)
			case k.is('p', 0) || k.is('K', 0):
				stepThread(-1)
			case k.is('r', 0):
				p := th.post
				write(&p)
			case k.is('a', 0):
				ask(th.post)
			case k.is('c', 0):
				write(nil)
			case k.is('O', 0) || k.is('o', modShift):
				openURL(th.post.AppURL)
			case k.is('R', 0) || k.is('r', modShift) || k.is('l', modCtrl):
				th.loading = true
				fetchThreadAsync(th.post.TopicID)
				draw(true)
			}
			return false
		}
		switch {
		case k.is('q', 0):
			return true
		case k.is(keyEscape, 0):
			if len(marked) > 0 {
				marked = map[int64]bool{}
				draw(false)
			} else if opt.floating {
				minimizeSelf()
			}
		case k.is('x', 0) && kind() == "imbox":
			if p, ok := selected(); ok {
				if marked[p.ID] {
					delete(marked, p.ID)
				} else {
					marked[p.ID] = true
				}
				if sel() < len(cur())-1 {
					moveSel(1)
				} else {
					draw(false)
				}
			}
		case (k.is('X', 0) || k.is('x', modShift)) && kind() == "imbox":
			// This one and every new thread below it: older than here.
			for _, p := range cur()[sel():] {
				if p.New {
					marked[p.ID] = true
				}
			}
			draw(false)
		case k.is('e', 0) && kind() == "imbox":
			ids := markedIDs()
			if len(ids) == 0 {
				if p, ok := selected(); ok && p.New {
					ids = []int64{p.ID}
				}
			}
			if len(ids) > 0 {
				confirmSeen = ids
				draw(false)
			}
		case k.is('A', 0) || k.is('a', modShift):
			triageNow()
		case k.is('j', 0) || k.is(keyDown, 0):
			moveSel(1)
		case k.is('k', 0) || k.is(keyUp, 0):
			moveSel(-1)
		case k.is(' ', 0) || k.is('d', 0) || k.is(keyPageDown, 0):
			moveSel(int(page / rowPitch))
		case k.is('b', 0) || k.is('u', 0) || k.is(keyPageUp, 0):
			moveSel(-int(page / rowPitch))
		case k.is('g', 0) || k.is(keyHome, 0):
			moveSel(-len(cur()))
		case k.is('G', 0) || k.is('g', modShift) || k.is(keyEnd, 0):
			moveSel(len(cur()))
		case k.is(keyEnter, 0) || k.is('l', 0) || k.is('o', 0) || k.is(keyRight, 0):
			if p, ok := selected(); ok {
				openThread(p)
			}
		case k.is(keyTab, 0) || k.is(']', 0):
			switchBox(boxIdx + 1)
		case k.is(keyTab, modShift) || k.is('[', 0):
			switchBox(boxIdx - 1)
		case k.code >= '1' && k.code <= rune('0'+len(boxes)) && k.mods == 0:
			switchBox(int(k.code - '1'))
		case k.is('r', 0):
			if p, ok := selected(); ok {
				write(&p)
			}
		case k.is('a', 0):
			if p, ok := selected(); ok {
				ask(p)
			}
		case k.is('c', 0):
			write(nil)
		case k.is('s', 0) && kind() == "imbox":
			// Between only what's new and the whole Imbox, on the same thread.
			var id int64
			if p, ok := selected(); ok {
				id = p.ID
			}
			showAll = !showAll
			sels[kind()] = max(0, indexOf(id))
			listOff[kind()] = 0
			snapList()
			keepSelVisible()
			shown = target
			draw(false)
		case k.is('O', 0) || k.is('o', modShift):
			if p, ok := selected(); ok {
				openURL(p.AppURL)
			}
		case k.is('R', 0) || k.is('r', modShift) || k.is('l', modCtrl):
			fetchBoxAsync(kind())
			draw(true)
		}
		return false
	}
	handleClick := func(c mouseClick) {
		g := lastGeom
		py := float64(c.y - 1)
		if c.x <= g.ws.cols && c.y <= g.ws.rows {
			py = float64((c.y-1)*g.cellH + g.cellH/2) // cell reports: the middle of the row
		}
		if py < float64(g.hdrPx) {
			if screen == screenThread {
				back()
			}
			return
		}
		if screen == screenThread && th != nil && docv != nil {
			px := float64(c.x - 1)
			if c.x <= g.ws.cols && c.y <= g.ws.rows {
				px = float64((c.x-1)*g.cellW + g.cellW/2)
			}
			if u := docv.linkAt(px/g.scale, (py-float64(g.hdrPx))/g.scale+shown); u != "" {
				openURL(u)
				setStatus("Opened "+shortURL(u), roleGray)
			}
			return
		}
		if screen != screenList {
			return
		}
		y := (py-float64(g.hdrPx))/g.scale + shown
		if i := lay().rowAt(y); i >= 0 {
			sels[kind()] = i
			hlTarget, hlShown = top(i), top(i)
			openThread(cur()[i])
		}
	}

	// --- the loop -------------------------------------------------------

	setTitle()
	snapList()
	draw(true)
	fetchBoxAsync(kind())

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	defer stopAnim()
	refresh := time.NewTicker(refreshEvery)
	defer refresh.Stop()
	recolor := time.NewTicker(colorPoll)
	defer recolor.Stop()
	var repaint <-chan time.Time
	for {
		select {
		case <-tick.C:
			if status != "" && time.Now().After(statusUntil) {
				status = ""
			}
			stamp := ""
			if st, err := os.Stat(triagePath()); err == nil {
				stamp = fmt.Sprint(st.ModTime().UnixNano(), st.Size())
			}
			if stamp != triageStamp {
				triageStamp, triage = stamp, loadTriage()
				triageVer++
			}
			draw(false)
		case <-refresh.C:
			fetchBoxAsync(kind())
			draw(false)
		case now := <-animC:
			step(now)
		case <-recolor.C:
			if opt.theme == "terminal" {
				queryColors(w)
				w.Flush()
			}
		case <-repaint:
			repaint = nil
			if p := colors.palette(opt.theme); p != pal {
				pal = p
				draw(true)
			}
		case <-winch:
			draw(true)
		case <-imgSettle:
			imgSettle = nil
			if th != nil {
				th.ver++
				draw(false)
			}
		case res := <-results:
			switch res.kind {
			case resBox:
				loading[res.box] = false
				if res.err != nil {
					errs[res.box] = res.err.Error()
				} else {
					// Keep what's on screen steady across the update: the
					// selection stays on its thread, and the rows in view stay
					// put, gliding to show what arrived above them.
					here := res.box == kind() && screen == screenList
					var keep, anchor int64
					anchorTop := 0.0
					if old := visible(res.box); len(old) > 0 {
						keep = old[max(0, min(sels[res.box], len(old)-1))].ID
						anchor, anchorTop = old[0].ID, layoutList(old, res.box == "imbox" && showAll).tops[0]
					}
					posts[res.box], errs[res.box] = res.posts, ""
					vers[res.box]++
					if keep != 0 && here {
						if i := indexOf(keep); i >= 0 {
							sels[res.box] = i
						}
					}
					if here {
						hlTarget = top(sel())
						hlShown = hlTarget
						if i := indexOf(anchor); anchor != 0 && i >= 0 {
							if lift := top(i) - anchorTop; lift > 0 {
								shown += lift
								hlShown = hlTarget // the selection moves with its row
								if target > 0.5 {
									target += lift
								} else {
									startAnim()
								}
							}
						}
					}
					setTitle()
				}
			case resImage:
				if th != nil && imgSettle == nil {
					imgSettle = time.After(80 * time.Millisecond)
				}
			case resThread:
				if th != nil && th.post.TopicID == res.topic {
					th.loading = false
					if res.err != nil {
						th.err = res.err.Error()
					} else {
						th.entries, th.err = res.entries, ""
					}
					th.ver++
				}
			case resWrite:
				ro := roleGray
				if res.ok {
					ro = roleAccent
				}
				setStatus(res.msg, ro)
				if res.ok {
					fetchBoxAsync(kind())
					if th != nil && th.post.TopicID == res.topic {
						th.loading = true
						fetchThreadAsync(res.topic)
					}
				}
			case resNote:
				setStatus(res.msg, roleGray)
			case resSeen:
				if res.err != nil {
					setStatus("Couldn't mark them seen: "+res.err.Error(), roleGray)
					break
				}
				for _, id := range res.ids {
					readNow(res.box, id)
					delete(marked, id)
				}
				setStatus(fmt.Sprintf("Marked %d seen", len(res.ids)), roleAccent)
				if res.box == kind() {
					sels[kind()] = min(sel(), max(0, len(cur())-1))
					keepSelVisible()
				}
				setTitle()
			case resRefresh:
				fetchBoxAsync(res.box)
			}
			draw(false)
		case ev, open := <-events:
			if !open {
				return nil
			}
			switch {
			case ev.scheme:
				queryColors(w)
				w.Flush()
			case ev.osc != "":
				if colors.apply(ev.osc) && repaint == nil && colors.palette(opt.theme) != pal {
					repaint = time.After(colorSettle)
				}
			case ev.scroll == wheelUp:
				scrollBy(-rowPitch / 2)
			case ev.scroll == wheelDown:
				scrollBy(rowPitch / 2)
			case ev.click != nil:
				handleClick(*ev.click)
			case ev.key.code != 0:
				if handleKey(ev.key) {
					return nil
				}
			}
		case <-snap:
			g := lastGeom
			offsetPx := int(math.Round(shown * g.scale))
			info := map[string]any{
				"screen": map[screenKind]string{screenList: "list", screenThread: "thread"}[screen],
				"box":    kind(), "posts": len(cur()), "sel": sel(), "new": newIn(kind()), "showAll": showAll,
				"shown": shown, "target": target, "hlShown": hlShown, "hlTarget": hlTarget,
				"offsetPx": offsetPx, "sheetTop": sheetTop, "sheetH": sheetH,
				"loading": loading[kind()], "error": errs[kind()], "status": status,
				"cells": fmt.Sprint(g.ws.cols, "x", g.ws.rows), "cellPx": fmt.Sprint(g.cellW, "x", g.cellH),
				"scale": g.scale, "hdrRows": g.hdrRows, "viewRows": g.rows, "theme": opt.theme, "floating": opt.floating,
			}
			if th != nil {
				info["thread"] = map[string]any{"topic": th.post.TopicID, "subject": th.post.Subject,
					"entries": len(th.entries), "loading": th.loading, "error": th.err}
			}
			writeSnapshot(g, pal, hdrImg, sheetImg, offsetPx-sheetTop, hlImg, hlRow, info)
		case <-quit:
			return nil
		}
	}
}
