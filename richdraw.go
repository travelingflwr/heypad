package main

// Laying out rich mail bodies (see rich.go) in the thread reader: styled
// text that wraps across style changes, links you can click, code, quotes,
// tables and images.

import (
	"image"
	"image/color"
	"image/draw"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"
)

// linkHit is a link's place in the document, in points.
type linkHit struct {
	x0, y0, x1, y1 float64
	url            string
}

// linkAt is the link under (x, y) points, or "".
func (d *doc) linkAt(x, y float64) string {
	for _, l := range d.links {
		if x >= l.x0-2 && x < l.x1+2 && y >= l.y0 && y < l.y1 {
			return l.url
		}
	}
	return ""
}

// --- styled text --------------------------------------------------------

func (r *renderer) sface(st fstyle, size float64) font.Face { return r.fonts.styled(st, size*r.scale) }

// cleanStyled drops what the font can't draw and folds whitespace to single
// spaces.
func (r *renderer) cleanStyled(s string, st fstyle) string {
	var b strings.Builder
	space := false
	for _, c := range s {
		if unicode.IsSpace(c) {
			space = b.Len() > 0
			continue
		}
		if !r.fonts.hasStyled(st, c) {
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

// cleanCode drops what the font can't draw but keeps spaces, with tabs as
// four.
func (r *renderer) cleanCode(s string) string {
	var b strings.Builder
	for _, c := range s {
		switch {
		case c == '\t':
			b.WriteString("    ")
		case c == ' ' || r.fonts.hasStyled(fsMono, c):
			b.WriteRune(c)
		}
	}
	return b.String()
}

func (r *renderer) widthStyled(s string, size float64, st fstyle) float64 {
	face := r.sface(st, size)
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

func (r *renderer) drawStyled(img *image.RGBA, s string, x, y, size float64, st fstyle, col color.RGBA) float64 {
	face := r.sface(st, size)
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

// --- wrapping spans -----------------------------------------------------

type piece struct {
	text  string
	st    fstyle
	size  float64
	sp    spanStyle
	link  string
	space bool // a space between words
	brk   bool // a hard line break
	w     float64
}

type lineRun struct {
	text string
	x, w float64
	st   fstyle
	size float64
	sp   spanStyle
	link string
}

func styleFor(sp spanStyle, base fstyle) fstyle {
	if sp&stCode != 0 {
		return fsMono
	}
	bold := base == fsBold || base == fsBoldItalic || sp&stBold != 0
	it := base == fsItalic || base == fsBoldItalic || sp&stItalic != 0
	switch {
	case bold && it:
		return fsBoldItalic
	case bold:
		return fsBold
	case it:
		return fsItalic
	}
	return fsRegular
}

// pieces splits spans into words, spaces and line breaks, each measured in
// its own font.
func (r *renderer) pieces(spans []span, size float64, base fstyle) []piece {
	var out []piece
	for _, sp := range spans {
		st := styleFor(sp.style, base)
		sz := size
		if st == fsMono {
			sz = size * 0.9
		}
		word := func(w string) {
			if w = r.cleanStyled(w, st); w != "" {
				out = append(out, piece{text: w, st: st, size: sz, sp: sp.style, link: sp.link, w: r.widthStyled(w, sz, st)})
			}
		}
		var b strings.Builder
		for _, c := range sp.text {
			switch {
			case c == '\n':
				word(b.String())
				b.Reset()
				out = append(out, piece{brk: true})
			case unicode.IsSpace(c):
				word(b.String())
				b.Reset()
				out = append(out, piece{text: " ", st: st, size: sz, sp: sp.style, link: sp.link, space: true, w: r.widthStyled(" ", sz, st)})
			default:
				b.WriteRune(c)
			}
		}
		word(b.String())
	}
	return out
}

// wrapPieces lays pieces into lines no wider than maxW points.
func (r *renderer) wrapPieces(ps []piece, maxW float64) [][]lineRun {
	var lines [][]lineRun
	var cur []lineRun
	x := 0.0
	var pending *piece
	put := func(p piece, w float64) {
		if n := len(cur); n > 0 {
			l := &cur[n-1]
			if l.st == p.st && l.size == p.size && l.sp == p.sp && l.link == p.link && math.Abs(l.x+l.w-x) < 0.01 {
				l.text += p.text
				l.w += w
				x += w
				return
			}
		}
		cur = append(cur, lineRun{text: p.text, x: x, w: w, st: p.st, size: p.size, sp: p.sp, link: p.link})
		x += w
	}
	newLine := func() {
		lines = append(lines, cur)
		cur, x, pending = nil, 0, nil
	}
	for i := range ps {
		p := ps[i]
		switch {
		case p.brk:
			newLine()
			continue
		case p.space:
			if len(cur) > 0 {
				pending = &ps[i]
			}
			continue
		}
		sw := 0.0
		if pending != nil {
			sw = pending.w
		}
		if x+sw+p.w <= maxW {
			if pending != nil {
				put(*pending, pending.w)
			}
			pending = nil
			put(p, p.w)
			continue
		}
		if len(cur) > 0 {
			newLine()
		}
		pending = nil
		// A word too long for a line (a URL, say) breaks between characters.
		for p.w > maxW {
			cut := len(p.text)
			for cut > 1 && r.widthStyled(p.text[:cut], p.size, p.st) > maxW {
				_, n := utf8.DecodeLastRuneInString(p.text[:cut])
				cut -= n
			}
			head := p
			head.text = p.text[:cut]
			put(head, r.widthStyled(head.text, p.size, p.st))
			newLine()
			p.text = p.text[cut:]
			p.w = r.widthStyled(p.text, p.size, p.st)
		}
		if p.text != "" {
			put(p, p.w)
		}
	}
	if len(cur) > 0 {
		lines = append(lines, cur)
	}
	return lines
}

// --- blocks -------------------------------------------------------------

const (
	quoteIndent = 12.0
	codeSize    = 12.0
	codeLineH   = 17.0
	imgMaxH     = 900.0
)

// flow lays spans out as wrapped lines starting at y, and returns the y
// below them.
func (r *renderer) flow(d *doc, spans []span, x, w, y, size, lh float64, base fstyle, ro role) float64 {
	for _, line := range r.wrapPieces(r.pieces(spans, size, base), w) {
		baseline := y + lh*0.5 + size*0.36
		for _, run := range line {
			rr := ro
			if run.link != "" {
				rr = roleAccent
				d.links = append(d.links, linkHit{x + run.x, y, x + run.x + run.w, y + lh, run.link})
			}
			if run.sp&stCode != 0 && strings.TrimSpace(run.text) != "" {
				d.items = append(d.items, docItem{kind: itemFill, x: x + run.x - 2, y: baseline - run.size*0.95, x1: x + run.x + run.w + 2, y1: baseline + run.size*0.35, size: 3, role: roleText, tint: 0.08})
			}
			d.items = append(d.items, docItem{kind: itemText, x: x + run.x, y: baseline, text: run.text, size: run.size, style: run.st, role: rr, strike: run.sp&stStrike != 0})
		}
		y += lh
	}
	return y
}

func headingSize(level int) float64 {
	switch level {
	case 1:
		return 19
	case 2:
		return 17
	case 3:
		return 15.5
	}
	return 14
}

// layoutBlocks lays blocks out in a column x to x+w from y, and returns the
// y below them.
func (r *renderer) layoutBlocks(d *doc, blocks []block, x, w, y float64) float64 {
	for i, b := range blocks {
		qx, qw := x+quoteIndent*float64(b.quote), w-quoteIndent*float64(b.quote)
		ro := roleText
		if b.quote > 0 {
			ro = roleGray
		}
		top := y
		gap := paraGap
		switch b.kind {
		case bkPara:
			y = r.flow(d, b.spans, qx, qw, y, bodySize, bodyLineH, fsRegular, ro)
		case bkHeading:
			if i > 0 {
				y += 6
			}
			size := headingSize(b.level)
			y = r.flow(d, b.spans, qx, qw, y, size, math.Round(size*1.4), fsBold, ro)
		case bkItem:
			mw := r.widthStyled(b.marker, bodySize, fsRegular)
			ind := 16*float64(b.level) + math.Max(14, mw+6)
			d.items = append(d.items, docItem{kind: itemText, x: qx + 16*float64(b.level) + 2, y: y + bodyLineH*0.5 + bodySize*0.36, text: b.marker, size: bodySize, role: ro})
			y = r.flow(d, b.spans, qx+ind, qw-ind, y, bodySize, bodyLineH, fsRegular, ro)
			if i+1 < len(blocks) && blocks[i+1].kind == bkItem {
				gap = 3
			}
		case bkCode:
			y = r.layoutCode(d, b.code, qx, qw, y)
		case bkRule:
			d.items = append(d.items, docItem{kind: itemRect, x: qx, y: y + 6, x1: qx + qw, y1: y + 7, role: roleGray})
			y += 12
		case bkTable:
			y = r.layoutTable(d, b, qx, qw, y, ro)
		case bkImage:
			y = r.layoutImage(d, b, qx, qw, y)
		}
		if y == top {
			continue // nothing drawn (a tracking pixel, say)
		}
		for q := 0; q < b.quote; q++ {
			bx := x + 2 + quoteIndent*float64(q)
			d.items = append(d.items, docItem{kind: itemRect, x: bx, y: top + 2, x1: bx + 2, y1: y + gap - 2, role: roleFaint})
		}
		y += gap
	}
	return y
}

func (r *renderer) layoutCode(d *doc, code []string, x, w, y float64) float64 {
	const pad = 9.0
	var lines []string
	for _, ln := range code {
		ln = r.cleanCode(ln)
		for r.widthStyled(ln, codeSize, fsMono) > w-2*pad && len(ln) > 1 {
			cut := len(ln)
			for cut > 1 && r.widthStyled(ln[:cut], codeSize, fsMono) > w-2*pad {
				_, n := utf8.DecodeLastRuneInString(ln[:cut])
				cut -= n
			}
			lines = append(lines, ln[:cut])
			ln = ln[cut:]
		}
		lines = append(lines, ln)
	}
	h := float64(len(lines))*codeLineH + 2*pad
	d.items = append(d.items, docItem{kind: itemFill, x: x, y: y, x1: x + w, y1: y + h, size: 6, role: roleText, tint: 0.06})
	for i, ln := range lines {
		if ln != "" {
			d.items = append(d.items, docItem{kind: itemText, x: x + pad, y: y + pad + float64(i)*codeLineH + 12.5, text: ln, size: codeSize, style: fsMono})
		}
	}
	return y + h
}

// layoutTable draws a table in columns sized to their content, or, when a
// table has too many columns for the pane, each row as a line of text.
func (r *renderer) layoutTable(d *doc, b block, x, w, y float64, ro role) float64 {
	const (
		gap    = 14.0
		pad    = 6.0
		minCol = 40.0
		maxNat = 280.0
	)
	n := 0
	for _, row := range b.rows {
		n = max(n, len(row))
	}
	if n == 0 {
		return y
	}
	if float64(n)*minCol+gap*float64(n-1) > w {
		for ri, row := range b.rows {
			var spans []span
			for ci, c := range row {
				if ci > 0 {
					spans = append(spans, span{text: "  ·  "})
				}
				spans = append(spans, c...)
			}
			base := fsRegular
			if b.head && ri == 0 {
				base = fsBold
			}
			y = r.flow(d, spans, x, w, y, bodySize, bodyLineH, base, ro)
		}
		return y
	}
	natural := make([]float64, n)
	for ri, row := range b.rows {
		base := fsRegular
		if b.head && ri == 0 {
			base = fsBold
		}
		for ci, c := range row {
			cw := 0.0
			for _, p := range r.pieces(c, bodySize, base) {
				cw += p.w
			}
			natural[ci] = math.Min(maxNat, math.Max(natural[ci], cw))
		}
	}
	avail := w - gap*float64(n-1)
	widths := make([]float64, n)
	total := 0.0
	for _, v := range natural {
		total += v
	}
	if total <= avail {
		for i, v := range natural {
			widths[i] = math.Max(v, 12)
		}
	} else {
		// Columns narrower than a fair share keep their width; the rest
		// share what's left.
		fixedCol := make([]bool, n)
		left, open := avail, n
		for changed := true; changed && open > 0; {
			changed = false
			share := left / float64(open)
			for i := range natural {
				if !fixedCol[i] && natural[i] <= share {
					fixedCol[i], widths[i] = true, natural[i]
					left -= natural[i]
					open--
					changed = true
				}
			}
		}
		for i := range widths {
			if !fixedCol[i] {
				widths[i] = math.Max(minCol, left/float64(max(1, open)))
			}
		}
	}
	tableW := gap * float64(n-1)
	for _, v := range widths {
		tableW += v
	}
	for ri, row := range b.rows {
		base := fsRegular
		if b.head && ri == 0 {
			base = fsBold
		}
		start := len(d.items)
		bottom := y + pad
		cx := x
		for ci := 0; ci < n; ci++ {
			if ci < len(row) {
				bottom = math.Max(bottom, r.flow(d, row[ci], cx, widths[ci], y+pad, bodySize, bodyLineH, base, ro))
			}
			cx += widths[ci] + gap
		}
		h := bottom - y + pad
		if b.head && ri == 0 {
			fill := docItem{kind: itemFill, x: x, y: y, x1: x + tableW, y1: y + h, size: 4, role: roleText, tint: 0.05}
			d.items = append(d.items[:start], append([]docItem{fill}, d.items[start:]...)...)
		}
		y += h
		if ri < len(b.rows)-1 {
			d.items = append(d.items, docItem{kind: itemRect, x: x, y: y, x1: x + tableW, y1: y + 1, role: roleGray})
		}
	}
	return y
}

func (r *renderer) layoutImage(d *doc, b block, x, w, y float64) float64 {
	e := r.imgs.get(b.src)
	state := imgFailed
	iw, ih := 0, 0
	if e != nil {
		state, iw, ih = e.status()
	}
	note := func(text string) float64 {
		return r.flow(d, []span{{text: text, style: stItalic}}, x, w, y, 12, 17, fsRegular, roleFaint)
	}
	switch state {
	case imgReady:
		if iw <= 2 || ih <= 2 {
			return y // a tracking pixel
		}
		dw := math.Min(float64(iw), w)
		dh := dw * float64(ih) / float64(iw)
		if dh > imgMaxH {
			dh, dw = imgMaxH, imgMaxH*float64(iw)/float64(ih)
		}
		d.items = append(d.items, docItem{kind: itemImage, x: x, y: y, x1: x + dw, y1: y + dh, src: b.src})
		if b.link != "" {
			d.links = append(d.links, linkHit{x, y, x + dw, y + dh, b.link})
		}
		return y + dh
	case imgLoading:
		h := 44.0
		d.items = append(d.items, docItem{kind: itemFill, x: x, y: y, x1: x + math.Min(w, 320), y1: y + h, size: 6, role: roleText, tint: 0.05})
		label := "Loading image…"
		if b.alt != "" {
			label = b.alt
		}
		d.items = append(d.items, docItem{kind: itemText, x: x + 12, y: y + h/2 + 4.3, text: r.fit(r.cleanStyled(label, fsItalic), 12, false, math.Min(w, 320)-24), size: 12, style: fsItalic, role: roleFaint})
		return y + h
	case imgBlocked:
		label := "Image not loaded"
		if b.alt != "" {
			label += ": " + b.alt
		}
		return note(label)
	}
	if b.alt != "" {
		return note(b.alt)
	}
	return y
}

// drawImage draws an image item scrolled up by offset points.
func (r *renderer) drawImage(img *image.RGBA, it docItem, offset float64) {
	e := r.imgs.get(it.src)
	if e == nil {
		return
	}
	pw, ph := int(math.Round((it.x1-it.x)*r.scale)), int(math.Round((it.y1-it.y)*r.scale))
	s := e.scaledTo(pw, ph)
	if s == nil {
		return
	}
	at := image.Pt(int(math.Round(it.x*r.scale)), int(math.Round((it.y-offset)*r.scale)))
	draw.Draw(img, s.Bounds().Add(at), s, image.Point{}, draw.Over)
}

// imageSources is every image src in d, for waiting on them.
func (d *doc) imageSources() []string {
	var out []string
	for _, it := range d.items {
		if it.kind == itemImage {
			out = append(out, it.src)
		}
	}
	return out
}

// shortURL is a link's host and the start of its path, for a status line.
func shortURL(u string) string {
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	if len(u) > 48 {
		u = u[:47] + "…"
	}
	return u
}
