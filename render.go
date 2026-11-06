package main

import (
	"image"
	"image/color"
	"image/draw"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

// Layout, in points.
const (
	padX     = 14.0 // content's left and right edge
	headerPt = 40.0 // the fixed bar at the top, before rounding to cells

	listTop  = 4.0
	rowPitch = 68.0
	avatarR  = 16.0
	rowTextX = padX + 2*avatarR + 12

	selInset  = 6.0 // the selection reaches this far from the pane's edges
	selRadius = 8.0

	bodySize  = 13.5
	bodyLineH = 20.0
	paraGap   = 8.0
)

type palette struct {
	bg, text, gray, faint, divider, accent, selBg color.RGBA
}

var lightPalette = palette{
	bg:      rgb(255, 255, 255),
	text:    rgb(18, 20, 23),
	gray:    rgb(104, 108, 115),
	faint:   rgb(150, 153, 158),
	divider: rgb(230, 232, 236),
	accent:  rgb(49, 95, 189),
	selBg:   rgb(232, 238, 249),
}

var darkPalette = palette{
	bg:      rgb(30, 31, 34),
	text:    rgb(226, 228, 231),
	gray:    rgb(152, 155, 161),
	faint:   rgb(118, 121, 127),
	divider: rgb(58, 60, 64),
	accent:  rgb(84, 136, 238),
	selBg:   rgb(37, 49, 72),
}

func rgb(r, g, b uint8) color.RGBA { return color.RGBA{r, g, b, 255} }

// grayMix is how far from the foreground toward the background each gray sits.
func grayMix(bg color.RGBA) (gray, faint, divider float64) {
	if luminance(bg) < 0.18 {
		return 0.38, 0.52, 0.85
	}
	return 0.45, 0.58, 0.9
}

// themedPalette derives the colors from a terminal theme.
func themedPalette(fg, bg, accent color.RGBA) palette {
	g, f, d := grayMix(bg)
	return palette{
		bg: bg, text: fg, accent: accent,
		gray:    mix(fg, bg, g),
		faint:   mix(fg, bg, f),
		divider: mix(fg, bg, d),
		selBg:   mix(accent, bg, 0.84),
	}
}

// seeThrough is the palette for drawing with no background: each gray is
// the foreground at the opacity that gives that gray over the background,
// so it blends with whatever the pane shows underneath.
func (p palette) seeThrough() palette {
	g, f, d := grayMix(p.bg)
	q := p
	q.gray = premul(p.text, 1-g)
	q.faint = premul(p.text, 1-f)
	q.divider = premul(p.text, 1-d)
	q.selBg = premul(p.accent, 0.16)
	return q
}

func mix(a, b color.RGBA, t float64) color.RGBA {
	l := func(x, y uint8) uint8 { return uint8(math.Round(float64(x) + (float64(y)-float64(x))*t)) }
	return color.RGBA{l(a.R, b.R), l(a.G, b.G), l(a.B, b.B), 255}
}

// premul is c (opaque) at opacity a, premultiplied as image.RGBA wants.
func premul(c color.RGBA, a float64) color.RGBA {
	a = math.Max(0, math.Min(1, a))
	f := func(v uint8) uint8 { return uint8(math.Round(float64(v) * a)) }
	return color.RGBA{f(c.R), f(c.G), f(c.B), uint8(math.Round(255 * a))}
}

func luminance(c color.RGBA) float64 {
	lin := func(v uint8) float64 {
		f := float64(v) / 255
		if f <= 0.04045 {
			return f / 12.92
		}
		return math.Pow((f+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(c.R) + 0.7152*lin(c.G) + 0.0722*lin(c.B)
}

func parseHex(s string) (color.RGBA, bool) {
	s = strings.TrimPrefix(s, "#")
	if len(s) != 6 {
		return color.RGBA{}, false
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return color.RGBA{}, false
	}
	return rgb(uint8(v>>16), uint8(v>>8), uint8(v)), true
}

type renderer struct {
	fonts *fontSet
	scale float64
	pal   palette
	clear bool // leave the background transparent
	now   time.Time
	// triage is an agent's verdict on threads, by box item ID, shown as a
	// chip on each row.
	triage map[int64]TriageItem
}

// tinted is c at opacity a over the background: see-through on a clear
// image, mixed on a painted one. The two look the same.
func (r *renderer) tinted(c color.RGBA, a float64) color.RGBA {
	if r.clear {
		return premul(c, a)
	}
	return mix(c, r.pal.bg, 1-a)
}

var triageLabels = map[string]string{"needs_you": "Needs you", "read": "Worth a read", "skip": "Skip"}

// chip draws a triage label ending at x, centered on cy, and returns its width.
func (r *renderer) chip(img *image.RGBA, label string, x, cy float64) float64 {
	text := triageLabels[label]
	bg, fg, bold := r.tinted(r.pal.text, 0.08), r.pal.gray, false
	switch label {
	case "needs_you":
		bg, fg, bold = r.tinted(r.pal.accent, 0.2), r.pal.accent, true
	case "skip":
		bg, fg = r.tinted(r.pal.text, 0.05), r.pal.faint
	}
	tw := r.width(text, 10.5, bold)
	w := tw + 12
	r.fillRoundRect(img, x-w, cy-8, x, cy+8, 8, bg)
	r.drawRaw(img, text, x-w+6, cy+3.8, 10.5, bold, fg)
	return w
}

// markBadge covers a row's avatar with a check, for a thread marked to be
// seen.
func (r *renderer) markBadge(img *image.RGBA, cx, cy float64) {
	r.fillCircle(img, cx, cy, avatarR, r.pal.accent)
	white := rgb(255, 255, 255)
	if luminance(r.pal.accent) > 0.5 {
		white = rgb(20, 20, 20)
	}
	r.check(img, cx, cy, 13, white)
}

func (r *renderer) paint(img *image.RGBA) {
	if !r.clear {
		draw.Draw(img, img.Bounds(), image.NewUniform(r.pal.bg), image.Point{}, draw.Src)
	}
}

// --- text ---------------------------------------------------------------

func (r *renderer) face(size float64, bold bool) font.Face { return r.fonts.face(bold, size*r.scale) }

// clean drops what the font can't draw and folds whitespace to single spaces.
func (r *renderer) clean(s string, bold bool) string {
	var b strings.Builder
	space := false
	for _, c := range s {
		if unicode.IsSpace(c) {
			space = b.Len() > 0
			continue
		}
		if !r.fonts.has(bold, c) {
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(c)
	}
	return b.String()
}

// width is how wide s (already clean) draws, in points.
func (r *renderer) width(s string, size float64, bold bool) float64 {
	face := r.face(size, bold)
	var w fixed.Int26_6
	prev := rune(-1)
	for _, c := range s {
		if prev >= 0 {
			w += face.Kern(prev, c)
		}
		adv, _ := face.GlyphAdvance(c)
		w += adv
		prev = c
	}
	return float64(w) / 64 / r.scale
}

// fit cuts s (already clean) with an ellipsis to fit maxW points.
func (r *renderer) fit(s string, size float64, bold bool, maxW float64) string {
	if maxW <= 0 || r.width(s, size, bold) <= maxW {
		return s
	}
	const ell = "…"
	for len(s) > 0 {
		_, n := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-n]
		if r.width(strings.TrimRight(s, " ,.;:-")+ell, size, bold) <= maxW {
			break
		}
	}
	return strings.TrimRight(s, " ,.;:-") + ell
}

// text draws s with its baseline at (x, y), cut to maxW when maxW > 0, and
// returns how wide it drew.
func (r *renderer) text(img *image.RGBA, s string, x, y, size float64, bold bool, col color.RGBA, maxW float64) float64 {
	s = r.fit(r.clean(s, bold), size, bold, maxW)
	return r.drawRaw(img, s, x, y, size, bold, col)
}

// textRight draws s ending at x.
func (r *renderer) textRight(img *image.RGBA, s string, x, y, size float64, bold bool, col color.RGBA) float64 {
	s = r.clean(s, bold)
	w := r.width(s, size, bold)
	r.drawRaw(img, s, x-w, y, size, bold, col)
	return w
}

func (r *renderer) drawRaw(img *image.RGBA, s string, x, y, size float64, bold bool, col color.RGBA) float64 {
	face := r.face(size, bold)
	src := image.NewUniform(col)
	dot := fixed.Point26_6{X: fixed.Int26_6(x * r.scale * 64), Y: fixed.Int26_6(math.Round(y*r.scale) * 64)}
	start := dot.X
	prev := rune(-1)
	for _, c := range s {
		if prev >= 0 {
			dot.X += face.Kern(prev, c)
		}
		dr, mask, mp, adv, ok := face.Glyph(dot, c)
		if ok {
			draw.DrawMask(img, dr, src, image.Point{}, mask, mp, draw.Over)
		}
		dot.X += adv
		prev = c
	}
	return float64(dot.X-start) / 64 / r.scale
}

// wrap breaks s into lines that fit maxW points. "\n" in s is a hard break;
// a word too long for a line (a URL, say) breaks between characters.
func (r *renderer) wrap(s string, size float64, bold bool, maxW float64) []string {
	var lines []string
	for _, hard := range strings.Split(s, "\n") {
		words := strings.Fields(r.clean(hard, bold))
		if len(words) == 0 {
			continue
		}
		line := ""
		for _, word := range words {
			try := word
			if line != "" {
				try = line + " " + word
			}
			if r.width(try, size, bold) <= maxW {
				line = try
				continue
			}
			if line != "" {
				lines = append(lines, line)
				line = ""
			}
			for r.width(word, size, bold) > maxW {
				cut := len(word)
				for cut > 1 && r.width(word[:cut], size, bold) > maxW {
					_, n := utf8.DecodeLastRuneInString(word[:cut])
					cut -= n
				}
				if cut <= 0 {
					break
				}
				lines = append(lines, word[:cut])
				word = word[cut:]
			}
			line = word
		}
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// --- shapes -------------------------------------------------------------

// pathBuilder collects polygons in points and fills them over only their
// bounding box.
type pathBuilder struct {
	r     *renderer
	img   *image.RGBA
	polys [][][2]float32
}

func (r *renderer) path(img *image.RGBA) *pathBuilder { return &pathBuilder{r: r, img: img} }

func (p *pathBuilder) poly(pts [][2]float64) {
	px := make([][2]float32, len(pts))
	for i, pt := range pts {
		px[i] = [2]float32{float32(pt[0] * p.r.scale), float32(pt[1] * p.r.scale)}
	}
	p.polys = append(p.polys, px)
}

func (p *pathBuilder) fill(col color.RGBA) {
	if len(p.polys) == 0 {
		return
	}
	minX, minY := float32(math.Inf(1)), float32(math.Inf(1))
	maxX, maxY := float32(math.Inf(-1)), float32(math.Inf(-1))
	for _, poly := range p.polys {
		for _, pt := range poly {
			minX, maxX = min(minX, pt[0]), max(maxX, pt[0])
			minY, maxY = min(minY, pt[1]), max(maxY, pt[1])
		}
	}
	box := image.Rect(int(math.Floor(float64(minX))), int(math.Floor(float64(minY))),
		int(math.Ceil(float64(maxX))), int(math.Ceil(float64(maxY)))).Intersect(p.img.Bounds())
	if box.Empty() {
		return
	}
	ras := vector.NewRasterizer(box.Dx(), box.Dy())
	ox, oy := float32(box.Min.X), float32(box.Min.Y)
	for _, poly := range p.polys {
		ras.MoveTo(poly[0][0]-ox, poly[0][1]-oy)
		for _, pt := range poly[1:] {
			ras.LineTo(pt[0]-ox, pt[1]-oy)
		}
		ras.ClosePath()
	}
	ras.Draw(p.img, box, image.NewUniform(col), image.Point{})
}

func arc(pts [][2]float64, cx, cy, rad, a0, a1 float64) [][2]float64 {
	n := int(math.Max(4, math.Ceil(math.Abs(a1-a0)*rad*4)))
	for i := 0; i <= n; i++ {
		a := a0 + (a1-a0)*float64(i)/float64(n)
		pts = append(pts, [2]float64{cx + rad*math.Cos(a), cy + rad*math.Sin(a)})
	}
	return pts
}

func roundRect(x0, y0, x1, y1, rad float64) [][2]float64 {
	rad = math.Max(0, math.Min(rad, math.Min(x1-x0, y1-y0)/2))
	var pts [][2]float64
	pts = arc(pts, x1-rad, y0+rad, rad, -math.Pi/2, 0)
	pts = arc(pts, x1-rad, y1-rad, rad, 0, math.Pi/2)
	pts = arc(pts, x0+rad, y1-rad, rad, math.Pi/2, math.Pi)
	pts = arc(pts, x0+rad, y0+rad, rad, math.Pi, 3*math.Pi/2)
	return pts
}

func (r *renderer) fillRoundRect(img *image.RGBA, x0, y0, x1, y1, rad float64, col color.RGBA) {
	p := r.path(img)
	p.poly(roundRect(x0, y0, x1, y1, rad))
	p.fill(col)
}

func (r *renderer) fillRect(img *image.RGBA, x0, y0, x1, y1 float64, col color.RGBA) {
	p := r.path(img)
	p.poly([][2]float64{{x0, y0}, {x1, y0}, {x1, y1}, {x0, y1}})
	p.fill(col)
}

func (r *renderer) fillCircle(img *image.RGBA, cx, cy, rad float64, col color.RGBA) {
	p := r.path(img)
	p.poly(arc(nil, cx, cy, rad, 0, 2*math.Pi))
	p.fill(col)
}

// avatar draws c's initials on their HEY avatar color.
func (r *renderer) avatar(img *image.RGBA, c Contact, name string, cx, cy, rad float64) {
	bg, ok := parseHex(c.AvatarColor)
	if !ok {
		bg = mix(r.pal.accent, rgb(255, 255, 255), 0.25)
	}
	r.fillCircle(img, cx, cy, rad, bg)
	ini := strings.ToUpper(c.Initials)
	if ini == "" {
		for _, ch := range name {
			if unicode.IsLetter(ch) || unicode.IsDigit(ch) {
				ini = strings.ToUpper(string(ch))
				break
			}
		}
	}
	if utf8.RuneCountInString(ini) > 2 {
		ini = string([]rune(ini)[:2])
	}
	fg := rgb(255, 255, 255)
	if luminance(bg) > 0.4 {
		fg = rgb(24, 26, 29)
	}
	size := rad * 0.78
	ini = r.clean(ini, true)
	w := r.width(ini, size, true)
	r.drawRaw(img, ini, cx-w/2, cy+size*0.36, size, true, fg)
}

// --- the box list -------------------------------------------------------

const sectionH = 30.0 // a section's heading, above its first row

// listLayout is where each thread's row sits, and the section headings.
type listLayout struct {
	tops   []float64 // each row's top, by index into the posts drawn
	heads  []listHead
	height float64
}

type listHead struct {
	y     float64 // the heading's top
	title string
	count int
}

// layoutList places rows, grouped under New For You and Previously Seen
// headings when sections is set (the Imbox in a full window).
func layoutList(posts []Posting, sections bool) listLayout {
	var l listLayout
	y := listTop
	inNew := -1 // which section the last row was in, -1 before any
	nNew := 0
	for _, p := range posts {
		if p.New {
			nNew++
		}
	}
	for _, p := range posts {
		section := 0
		if p.New {
			section = 1
		}
		if sections && section != inNew {
			if inNew != -1 {
				y += 6
			}
			h := listHead{y: y, title: "Previously Seen", count: len(posts) - nNew}
			if p.New {
				h.title, h.count = "New For You", nNew
			}
			l.heads = append(l.heads, h)
			y += sectionH
			inNew = section
		}
		l.tops = append(l.tops, y)
		y += rowPitch
	}
	if len(posts) > 0 {
		l.height = y + 8
	}
	return l
}

// rowAt is the row under y points down the content, or -1.
func (l listLayout) rowAt(y float64) int {
	for i, t := range l.tops {
		if y >= t && y < t+rowPitch {
			return i
		}
	}
	return -1
}

// drawList draws the rows that fall in the image, with the content scrolled
// down by offset points. msg shows when there are no rows; done draws it as
// the all-caught-up state.
func (r *renderer) drawList(img *image.RGBA, posts []Posting, lay listLayout, offset float64, msg string, done bool) {
	widthPt := float64(img.Bounds().Dx()) / r.scale
	viewH := float64(img.Bounds().Dy()) / r.scale
	if len(posts) == 0 {
		if done {
			// A check in a ring, and the message under it.
			cx, cy := widthPt/2, math.Max(70, viewH*0.3)-offset
			p := r.path(img)
			p.poly(arc(nil, cx, cy, 22, 0, 2*math.Pi))
			p.poly(reversed(arc(nil, cx, cy, 20.5, 0, 2*math.Pi)))
			p.fill(r.pal.accent)
			r.check(img, cx, cy, 9, r.pal.accent)
			w := r.width(r.clean(msg, true), 15, true)
			r.drawRaw(img, r.clean(msg, true), cx-w/2, cy+50, 15, true, r.pal.text)
			sub := "Nothing new in your Imbox."
			sw := r.width(sub, 12.5, false)
			r.drawRaw(img, sub, cx-sw/2, cy+70, 12.5, false, r.pal.gray)
			return
		}
		if msg != "" {
			for i, ln := range r.wrap(msg, 13, false, widthPt-2*padX) {
				r.drawRaw(img, ln, padX, 34+float64(i)*19-offset, 13, false, r.pal.gray)
			}
		}
		return
	}
	for _, h := range lay.heads {
		y := h.y - offset
		if y < -sectionH || y > viewH {
			continue
		}
		title := strings.ToUpper(h.title)
		tw := r.drawRaw(img, r.clean(title, true), padX, y+20, 11, true, r.pal.gray)
		r.drawRaw(img, strconv.Itoa(h.count), padX+tw+7, y+20, 11, false, r.pal.faint)
	}
	for i, p := range posts {
		y0 := lay.tops[i] - offset
		if y0 < -rowPitch {
			continue
		}
		if y0 > viewH {
			break
		}
		// No divider under a section's last row.
		last := i == len(posts)-1 || posts[i+1].New != p.New && len(lay.heads) > 1
		r.postRow(img, p, y0, widthPt, last, p.New)
	}
}

func reversed(pts [][2]float64) [][2]float64 {
	out := make([][2]float64, len(pts))
	for i, p := range pts {
		out[len(pts)-1-i] = p
	}
	return out
}

// check strokes a checkmark size points wide, centered at (cx, cy).
func (r *renderer) check(img *image.RGBA, cx, cy, size float64, col color.RGBA) {
	pts := [][2]float64{{cx - size*0.55, cy + size*0.02}, {cx - size*0.15, cy + size*0.42}, {cx + size*0.6, cy - size*0.38}}
	const w = 1.0
	p := r.path(img)
	for i := 0; i+1 < len(pts); i++ {
		a, b := pts[i], pts[i+1]
		dx, dy := b[0]-a[0], b[1]-a[1]
		l := math.Hypot(dx, dy)
		nx, ny := -dy/l*w, dx/l*w
		ex, ey := dx/l*w*0.6, dy/l*w*0.6
		p.poly([][2]float64{{a[0] - nx - ex, a[1] - ny - ey}, {b[0] - nx + ex, b[1] - ny + ey},
			{b[0] + nx + ex, b[1] + ny + ey}, {a[0] + nx - ex, a[1] + ny - ey}})
	}
	p.fill(col)
}

func (r *renderer) postRow(img *image.RGBA, p Posting, y0, widthPt float64, last, fresh bool) {
	right := widthPt - padX
	r.avatar(img, p.Creator, p.sender(), padX+avatarR, y0+30, avatarR)

	// New For You: the date in the accent with a dot before it.
	date := shortDate(p.ActiveAt, r.now)
	dateCol := r.pal.faint
	if fresh {
		dateCol = r.pal.accent
	}
	dw := r.textRight(img, date, right, y0+23, 12, false, dateCol)
	if fresh {
		r.fillCircle(img, right-dw-8, y0+19, 3.5, r.pal.accent)
		dw += 13
	}
	if t, ok := r.triage[p.ID]; ok && right-dw-rowTextX > 150 {
		dw += r.chip(img, t.Label, right-dw-8, y0+19) + 8
	}
	nameMax := right - rowTextX - dw - 10
	nw := r.text(img, p.sender(), rowTextX, y0+23, 13.5, true, r.pal.text, nameMax)
	if p.Count > 1 && rowTextX+nw+24 < right-dw-10 {
		r.drawRaw(img, strconv.Itoa(p.Count), rowTextX+nw+6, y0+23, 12, false, r.pal.faint)
	}
	subject := p.Subject
	if strings.TrimSpace(r.clean(subject, false)) == "" {
		subject = "(no subject)"
	}
	r.text(img, subject, rowTextX, y0+41, 13.5, false, r.pal.text, right-rowTextX)
	r.text(img, inline(p.Summary), rowTextX, y0+58, 12, false, r.pal.gray, right-rowTextX)
	if !last {
		r.fillRect(img, rowTextX, y0+rowPitch-0.5, widthPt, y0+rowPitch+0.5, r.pal.divider)
	}
}

// selection draws the highlight behind a row whose top is y0 points down img.
func (r *renderer) selection(img *image.RGBA, y0 float64) {
	widthPt := float64(img.Bounds().Dx()) / r.scale
	r.fillRoundRect(img, selInset, y0+3, widthPt-selInset, y0+rowPitch-3, selRadius, r.pal.selBg)
}

// --- the thread reader --------------------------------------------------

type role int

const (
	roleText role = iota
	roleGray
	roleFaint
	roleAccent
)

func (r *renderer) color(ro role) color.RGBA {
	switch ro {
	case roleGray:
		return r.pal.gray
	case roleFaint:
		return r.pal.faint
	case roleAccent:
		return r.pal.accent
	}
	return r.pal.text
}

type itemKind int

const (
	itemText itemKind = iota
	itemTextRight
	itemAvatar
	itemRect
)

// docItem is one thing the reader draws: a line of text (baseline at y), an
// avatar (centered at x, y), or a rect (x, y to x1, y1).
type docItem struct {
	kind    itemKind
	x, y    float64
	x1, y1  float64
	text    string
	size    float64
	bold    bool
	role    role
	contact Contact
}

type doc struct {
	items  []docItem
	height float64
}

// layoutThread lays a thread out for a pane widthPt points wide.
func (r *renderer) layoutThread(p Posting, entries []Entry, widthPt float64, note string) *doc {
	d := &doc{}
	pad := 16.0
	maxW := widthPt - 2*pad
	y := 14.0
	add := func(it docItem) { d.items = append(d.items, it) }

	subject := p.Subject
	if strings.TrimSpace(subject) == "" {
		subject = "(no subject)"
	}
	for _, ln := range r.wrap(subject, 18, true, maxW) {
		add(docItem{kind: itemText, x: pad, y: y + 18, text: ln, size: 18, bold: true})
		y += 24
	}
	y += 6
	if note != "" {
		for _, ln := range r.wrap(note, 12.5, false, maxW) {
			add(docItem{kind: itemText, x: pad, y: y + 13, text: ln, size: 12.5, role: roleGray})
			y += 18
		}
		y += 6
	}
	for i, e := range entries {
		if i > 0 {
			y += 10
			add(docItem{kind: itemRect, x: pad, y: y, x1: widthPt - pad, y1: y + 1, role: roleFaint})
			y += 16
		} else {
			y += 6
		}
		who := e.Creator
		if who.AvatarColor == "" && strings.EqualFold(who.Email, p.Creator.Email) {
			who.AvatarColor, who.Initials = p.Creator.AvatarColor, firstNonEmpty(who.Initials, p.Creator.Initials)
		}
		add(docItem{kind: itemAvatar, x: pad + 14, y: y + 14, size: 14, contact: who, text: e.sender()})
		hx := pad + 28 + 10
		when := e.when()
		date := shortDate(when, r.now)
		if !when.IsZero() && shortDate(when, r.now) == when.Local().Format("3:04 PM") {
			date = "Today " + date
		} else if !when.IsZero() {
			date = when.Local().Format("Jan 2, 3:04 PM")
		}
		dw := r.width(r.clean(date, false), 12, false)
		add(docItem{kind: itemTextRight, x: widthPt - pad, y: y + 12, text: date, size: 12, role: roleFaint})
		add(docItem{kind: itemText, x: hx, y: y + 12, text: r.fit(r.clean(e.sender(), true), 13.5, true, widthPt-pad-hx-dw-10), size: 13.5, bold: true})
		if e.Creator.Email != "" && !strings.EqualFold(e.Creator.Email, e.sender()) {
			add(docItem{kind: itemText, x: hx, y: y + 28, text: r.fit(r.clean(e.Creator.Email, false), 12, false, widthPt-pad-hx), size: 12, role: roleGray})
		}
		y += 40

		paras := cleanBody(firstNonEmpty(e.Body, e.Summary))
		for _, pr := range paras {
			x, w, size, bold, ro, lh := pad, maxW, bodySize, false, roleText, bodyLineH
			switch pr.kind {
			case paraQuote:
				x, w, ro = pad+12, maxW-12, roleGray
			case paraHeading:
				size, bold, lh = 15, true, 21
			case paraBullet:
				x, w = pad+14, maxW-14
				add(docItem{kind: itemText, x: pad + 2, y: y + 14, text: "•", size: size})
			}
			lines := r.wrap(pr.text, size, bold, w)
			if len(lines) == 0 {
				continue
			}
			top := y
			for _, ln := range lines {
				add(docItem{kind: itemText, x: x, y: y + 14, text: ln, size: size, bold: bold, role: ro})
				y += lh
			}
			if pr.kind == paraQuote {
				add(docItem{kind: itemRect, x: pad + 2, y: top + 2, x1: pad + 4, y1: y - 2, role: roleFaint})
			}
			y += paraGap
		}
	}
	d.height = y + 24
	return d
}

// drawDoc draws the part of d in img, scrolled down by offset points.
func (r *renderer) drawDoc(img *image.RGBA, d *doc, offset float64) {
	viewH := float64(img.Bounds().Dy()) / r.scale
	for _, it := range d.items {
		y := it.y - offset
		switch it.kind {
		case itemText, itemTextRight:
			if y < -it.size || y > viewH+it.size {
				continue
			}
			if it.kind == itemTextRight {
				r.textRight(img, it.text, it.x, y, it.size, it.bold, r.color(it.role))
			} else {
				r.drawRaw(img, it.text, it.x, y, it.size, it.bold, r.color(it.role))
			}
		case itemAvatar:
			if y < -40 || y > viewH+40 {
				continue
			}
			r.avatar(img, it.contact, it.text, it.x, y, it.size)
		case itemRect:
			if it.y1-offset < 0 || y > viewH {
				continue
			}
			col := r.pal.divider
			if it.role == roleFaint && it.x1-it.x < 4 {
				col = r.pal.faint // a quote's bar
			}
			r.fillRect(img, it.x, y, it.x1, it.y1-offset, col)
		}
	}
}

// --- the header ---------------------------------------------------------

type header struct {
	badge     string // after the title, in the accent: "3 new"
	left      string
	leftRole  role
	leftBold  bool
	right     string
	rightRole role
}

func (r *renderer) drawHeader(img *image.RGBA, h header) {
	widthPt := float64(img.Bounds().Dx()) / r.scale
	hPt := float64(img.Bounds().Dy()) / r.scale
	size := 13.5
	if h.leftBold {
		size = 16
	}
	cy := hPt / 2
	rw := 0.0
	if h.right != "" {
		// Hints are separated by three spaces; each is drawn on its own with
		// a gap between, dropping from the left when the pane is narrow.
		maxR := (widthPt - 2*padX) * 0.6
		const gap = 14.0
		var parts []string
		var widths []float64
		for _, part := range strings.Split(h.right, "   ") {
			if c := r.clean(part, false); c != "" {
				parts, widths = append(parts, c), append(widths, r.width(c, 12, false))
			}
		}
		total := func() float64 {
			t := 0.0
			for _, w := range widths {
				t += w
			}
			return t + gap*float64(max(0, len(widths)-1))
		}
		for len(parts) > 1 && total() > maxR {
			parts, widths = parts[1:], widths[1:]
		}
		if len(parts) == 1 && widths[0] > maxR {
			parts[0] = r.fit(parts[0], 12, false, maxR)
			widths[0] = r.width(parts[0], 12, false)
		}
		rw = total()
		x := widthPt - padX - rw
		for i, part := range parts {
			r.drawRaw(img, part, x, cy+4.2, 12, false, r.color(h.rightRole))
			x += widths[i] + gap
		}
	}
	lw := r.text(img, h.left, padX, cy+size*0.36, size, h.leftBold, r.color(h.leftRole), widthPt-2*padX-rw-12)
	if h.badge != "" && padX+lw+80 < widthPt-padX-rw {
		bx := padX + lw + 10
		r.fillCircle(img, bx+3.5, cy, 3.5, r.pal.accent)
		r.drawRaw(img, r.clean(h.badge, false), bx+11, cy+4.2, 12, false, r.pal.accent)
	}
	r.fillRect(img, 0, hPt-1, widthPt, hPt, r.pal.divider)
}
