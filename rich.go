package main

// Mail bodies as a small rich document: blocks (paragraphs, headings, list
// items, code, rules, tables, images) made of styled spans. HEY's Markdown
// turns into it here; mail from other accounts (HTML over IMAP) will turn
// into the same thing, so the reader draws every account the same way.

import (
	"html"
	"regexp"
	"strings"
	"unicode"
)

type spanStyle uint8

const (
	stBold spanStyle = 1 << iota
	stItalic
	stCode
	stStrike
)

// span is a run of text in one style. A span with img set is an inline
// image: img is its source and text its alt text.
type span struct {
	text  string
	style spanStyle
	link  string
	img   string
}

type blockKind int

const (
	bkPara blockKind = iota
	bkHeading
	bkItem
	bkCode
	bkRule
	bkTable
	bkImage
)

type block struct {
	kind   blockKind
	level  int    // a heading's level (1-6), or a list item's nesting from 0
	marker string // a list item's bullet or number, such as "•" or "3."
	quote  int    // how many quotes deep the block sits
	spans  []span
	code   []string   // a code block's lines, as written
	rows   [][][]span // a table's rows of cells
	head   bool       // the table's first row is its header
	src    string     // an image's source
	alt    string     // an image's alt text
	link   string     // where an image links to
}

var (
	reFence    = regexp.MustCompile("^(```|~~~)")
	reHeading  = regexp.MustCompile(`^(#{1,6})\s+(.*?)\s*#*$`)
	reListItem = regexp.MustCompile(`^([-*+]|\d{1,3}[.)])\s+`)
	reTag      = regexp.MustCompile(`</?[a-zA-Z][a-zA-Z0-9]*(\s[^<>]*)?/?>`)
	reBareURL  = regexp.MustCompile(`https?://[^\s<>"]+`)
)

// parseRich turns a message's Markdown into blocks.
func parseRich(md string) []block {
	var out []block
	var cur *block       // the paragraph, heading or list item being built
	var curText []string // its lines, joined when it's done
	var hard []bool      // whether each line ends in a hard break
	var table [][]string
	tableQuote, tableHead := 0, false
	var code *block

	flush := func() {
		if cur == nil {
			return
		}
		var b strings.Builder
		for i, ln := range curText {
			if i > 0 {
				if hard[i-1] {
					b.WriteString("\n")
				} else {
					b.WriteString(" ")
				}
			}
			b.WriteString(ln)
		}
		out = append(out, splitImages(*cur, parseInline(b.String()))...)
		cur, curText, hard = nil, nil, nil
	}
	flushTable := func() {
		if table != nil {
			out = append(out, makeTable(table, tableHead, tableQuote)...)
		}
		table, tableHead = nil, false
	}

	for _, raw := range strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n") {
		if code != nil {
			if reFence.MatchString(strings.TrimSpace(raw)) {
				out = append(out, *code)
				code = nil
				continue
			}
			code.code = append(code.code, strings.TrimRight(raw, " \t"))
			continue
		}
		breakAfter := strings.HasSuffix(raw, "  ") || strings.HasSuffix(raw, "\\")
		// Indentation decides list nesting; tabs count as four spaces.
		indent := 0
		for _, c := range raw {
			if c == ' ' {
				indent++
			} else if c == '\t' {
				indent += 4
			} else {
				break
			}
		}
		s := strings.TrimSpace(strings.TrimSuffix(strings.TrimRight(raw, " "), "\\"))
		quote := 0
		for strings.HasPrefix(s, ">") {
			quote++
			s = strings.TrimSpace(s[1:])
		}
		if reFence.MatchString(s) {
			flush()
			flushTable()
			code = &block{kind: bkCode, quote: quote}
			continue
		}
		if strings.HasPrefix(s, "|") {
			flush()
			if table != nil && tableQuote != quote {
				flushTable()
			}
			if reTableSep.MatchString(s) && strings.Contains(s, "-") {
				if len(table) == 1 {
					tableHead = true
				}
				continue
			}
			table = append(table, splitRow(s))
			tableQuote = quote
			continue
		}
		flushTable()
		if s == "" {
			flush()
			continue
		}
		if reRule.MatchString(s) {
			flush()
			out = append(out, block{kind: bkRule, quote: quote})
			continue
		}
		if m := reHeading.FindStringSubmatch(s); m != nil {
			flush()
			cur = &block{kind: bkHeading, level: len(m[1]), quote: quote}
			curText, hard = []string{m[2]}, []bool{false}
			flush()
			continue
		}
		if m := reListItem.FindString(s); m != "" {
			flush()
			marker := "•"
			if n := strings.TrimSpace(m); n[0] >= '0' && n[0] <= '9' {
				marker = strings.TrimRight(n, ".)") + "."
			}
			level := min(indent/2, 3)
			if marker == "•" && level > 0 {
				marker = "◦"
			}
			cur = &block{kind: bkItem, level: level, marker: marker, quote: quote}
			curText, hard = []string{s[len(m):]}, []bool{breakAfter}
			continue
		}
		// A line under a list item, or the next line of a paragraph, joins
		// it; a change of quote depth starts a new paragraph.
		if cur != nil && cur.quote != quote {
			flush()
		}
		if cur == nil {
			cur = &block{kind: bkPara, quote: quote}
		}
		curText, hard = append(curText, s), append(hard, breakAfter)
	}
	if code != nil {
		out = append(out, *code)
	}
	flush()
	flushTable()
	return out
}

// splitRow splits a Markdown table row into its cells.
func splitRow(s string) []string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "|")
	s = strings.TrimSuffix(s, "|")
	var cells []string
	var b strings.Builder
	esc := false
	for _, c := range s {
		switch {
		case esc:
			b.WriteRune(c)
			esc = false
		case c == '\\':
			b.WriteRune(c)
			esc = true
		case c == '|':
			cells = append(cells, strings.TrimSpace(b.String()))
			b.Reset()
		default:
			b.WriteRune(c)
		}
	}
	return append(cells, strings.TrimSpace(b.String()))
}

// makeTable builds a table, dropping columns that are empty in every row.
// Mail uses tables for layout far more than for data, so a table left with
// one column is just its cells, one paragraph each.
func makeTable(rows [][]string, head bool, quote int) []block {
	cols := 0
	for _, r := range rows {
		cols = max(cols, len(r))
	}
	keep := make([]bool, cols)
	for _, r := range rows {
		for i, c := range r {
			if strings.TrimSpace(inline(c)) != "" || strings.Contains(c, "![") {
				keep[i] = true
			}
		}
	}
	var cells [][][]span
	for _, r := range rows {
		var row [][]span
		for i := 0; i < cols; i++ {
			if !keep[i] {
				continue
			}
			c := ""
			if i < len(r) {
				c = r[i]
			}
			row = append(row, parseInline(c))
		}
		cells = append(cells, row)
	}
	n := 0
	for _, k := range keep {
		if k {
			n++
		}
	}
	if n <= 1 {
		var out []block
		for _, row := range cells {
			for _, c := range row {
				out = append(out, splitImages(block{kind: bkPara, quote: quote}, c)...)
			}
		}
		return out
	}
	return []block{{kind: bkTable, rows: cells, head: head, quote: quote}}
}

// splitImages puts b's spans in b, taking inline images out into image
// blocks of their own, and drops what's left empty.
func splitImages(b block, spans []span) []block {
	var out []block
	part := b
	emit := func() {
		if spansBlank(part.spans) {
			part.spans = nil
			return
		}
		out = append(out, part)
		part = b
		part.spans = nil
		// A list item's text after an image continues the item, unmarked.
		if b.kind == bkItem {
			part.kind, part.marker = bkPara, ""
		}
	}
	for _, sp := range spans {
		if sp.img == "" {
			part.spans = append(part.spans, sp)
			continue
		}
		emit()
		out = append(out, block{kind: bkImage, quote: b.quote, src: sp.img, alt: sp.text, link: sp.link})
	}
	emit()
	return out
}

func spansBlank(spans []span) bool {
	for _, sp := range spans {
		if strings.TrimSpace(sp.text) != "" {
			return false
		}
	}
	return true
}

// parseInline turns one paragraph of Markdown into spans.
func parseInline(s string) []span {
	s = strings.NewReplacer("<br>", "\n", "<br/>", "\n", "<br />", "\n").Replace(s)
	p := &inlineParser{}
	p.parse(s, 0, "")
	return p.finish()
}

type inlineParser struct {
	out []span
}

func (p *inlineParser) add(text string, st spanStyle, link string) {
	if text == "" {
		return
	}
	if n := len(p.out); n > 0 && p.out[n-1].img == "" && p.out[n-1].style == st && p.out[n-1].link == link {
		p.out[n-1].text += text
		return
	}
	p.out = append(p.out, span{text: text, style: st, link: link})
}

// parse reads s in style st, inside a link to link when it's set.
func (p *inlineParser) parse(s string, st spanStyle, link string) {
	var lit strings.Builder
	emit := func() {
		p.add(lit.String(), st, link)
		lit.Reset()
	}
	for i := 0; i < len(s); {
		c := s[i]
		rest := s[i:]
		switch {
		case c == '\\' && i+1 < len(s) && strings.ContainsRune("\\`*_{}[]()#+-.!|~<>", rune(s[i+1])):
			lit.WriteByte(s[i+1])
			i += 2
			continue
		case c == '`':
			n := len(rest) - len(strings.TrimLeft(rest, "`"))
			if end := strings.Index(rest[n:], rest[:n]); end >= 0 {
				emit()
				code := rest[n : n+end]
				if strings.HasPrefix(code, " ") && strings.HasSuffix(code, " ") && len(strings.TrimSpace(code)) > 0 {
					code = code[1 : len(code)-1]
				}
				p.add(code, st|stCode, link)
				i += 2*n + end
				continue
			}
		case c == '!' && strings.HasPrefix(rest, "!["):
			if text, url, n, ok := linkAt(rest[1:]); ok {
				emit()
				p.out = append(p.out, span{text: inline(text), img: url, link: link})
				i += 1 + n
				continue
			}
		case c == '[':
			if text, url, n, ok := linkAt(rest); ok {
				emit()
				if link != "" {
					url = link // no links inside links
				}
				p.parse(text, st, url)
				i += n
				continue
			}
		case c == '<':
			if end := strings.IndexByte(rest, '>'); end > 0 {
				inner := rest[1:end]
				if strings.HasPrefix(inner, "http://") || strings.HasPrefix(inner, "https://") || strings.HasPrefix(inner, "mailto:") {
					emit()
					p.add(strings.TrimPrefix(inner, "mailto:"), st, firstNonEmpty(link, inner))
					i += end + 1
					continue
				}
				if m := reTag.FindString(rest); m != "" && strings.HasPrefix(rest, m) {
					i += len(m) // other HTML tags go
					continue
				}
			}
		case c == '~' && strings.HasPrefix(rest, "~~"):
			if end := strings.Index(rest[2:], "~~"); end > 0 {
				emit()
				p.parse(rest[2:2+end], st|stStrike, link)
				i += end + 4
				continue
			}
		case c == '*' || c == '_':
			if n, end, ok := emphasisAt(s, i); ok {
				emit()
				add := stItalic
				if n == 2 {
					add = stBold
				} else if n == 3 {
					add = stBold | stItalic
				}
				p.parse(s[i+n:end], st|add, link)
				i = end + n
				continue
			}
		}
		lit.WriteByte(c)
		i++
	}
	emit()
}

// linkAt reads "[text](url)" at the start of s.
func linkAt(s string) (text, url string, n int, ok bool) {
	depth, close := 0, -1
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				close = i
			}
		}
		if close >= 0 {
			break
		}
	}
	if close < 0 || close+1 >= len(s) || s[close+1] != '(' {
		return "", "", 0, false
	}
	depth = 0
	for j := close + 1; j < len(s); j++ {
		switch s[j] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				dest := strings.TrimSpace(s[close+2 : j])
				// Drop a title: (url "title").
				if k := strings.IndexAny(dest, " \t"); k > 0 {
					dest = dest[:k]
				}
				dest = strings.Trim(dest, "<>")
				return s[1:close], html.UnescapeString(dest), j + 1, true
			}
		}
	}
	return "", "", 0, false
}

// emphasisAt finds emphasis opening at s[i]: its delimiter length (1-3) and
// where its closing delimiter starts. An underscore inside a word isn't
// emphasis, and the delimiters must hug the text. A run with no closing
// delimiter as long tries shorter ones, so "***a**" is a star and bold "a".
func emphasisAt(s string, i int) (n, end int, ok bool) {
	c := s[i]
	run := 0
	for run < 3 && i+run < len(s) && s[i+run] == c {
		run++
	}
	if i+run >= len(s) || s[i+run] == ' ' {
		return 0, 0, false
	}
	if c == '_' && i > 0 && isWordByte(s[i-1]) {
		return 0, 0, false
	}
	for n = run; n >= 1; n-- {
		delim := strings.Repeat(string(c), n)
		for from := i + run; ; {
			k := strings.Index(s[from:], delim)
			if k < 0 {
				break
			}
			j := from + k
			after := j + n
			if s[j-1] != ' ' && (after >= len(s) || s[after] != c) &&
				!(c == '_' && after < len(s) && isWordByte(s[after])) {
				return n, j, true
			}
			from = j + 1
		}
	}
	return 0, 0, false
}

func isWordByte(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= 0x80
}

// finish decodes entities, drops the invisible padding marketing mail uses,
// folds runs of spaces, and links bare URLs.
func (p *inlineParser) finish() []span {
	var out []span
	for _, sp := range p.out {
		if sp.img != "" {
			out = append(out, sp)
			continue
		}
		t := html.UnescapeString(sp.text)
		t = invisible.Replace(t)
		if sp.style&stCode == 0 {
			t = foldSpaces(t)
		}
		if t == "" {
			continue
		}
		if sp.link != "" || sp.style&stCode != 0 {
			out = append(out, span{text: t, style: sp.style, link: sp.link})
			continue
		}
		last := 0
		for _, m := range reBareURL.FindAllStringIndex(t, -1) {
			url := strings.TrimRight(t[m[0]:m[1]], ".,;:!?)]'\"")
			end := m[0] + len(url)
			if m[0] > last {
				out = append(out, span{text: t[last:m[0]], style: sp.style})
			}
			out = append(out, span{text: url, style: sp.style, link: url})
			last = end
		}
		if last < len(t) {
			out = append(out, span{text: t[last:], style: sp.style})
		}
	}
	// Trim the paragraph's outer whitespace.
	for len(out) > 0 && out[0].img == "" {
		out[0].text = strings.TrimLeftFunc(out[0].text, unicode.IsSpace)
		if out[0].text != "" {
			break
		}
		out = out[1:]
	}
	for len(out) > 0 && out[len(out)-1].img == "" {
		n := len(out) - 1
		out[n].text = strings.TrimRightFunc(out[n].text, unicode.IsSpace)
		if out[n].text != "" {
			break
		}
		out = out[:n]
	}
	return out
}

// foldSpaces folds runs of spaces and tabs (not newlines) to one space, and
// drops spaces around a newline.
func foldSpaces(s string) string {
	var b strings.Builder
	space := false
	for _, c := range s {
		if c == '\n' {
			b.WriteRune('\n')
			space = false
			continue
		}
		if c == ' ' || c == '\t' || c == ' ' {
			space = true
			continue
		}
		if space {
			if !strings.HasSuffix(b.String(), "\n") {
				b.WriteByte(' ')
			}
			space = false
		}
		b.WriteRune(c)
	}
	if space && !strings.HasSuffix(b.String(), "\n") {
		b.WriteByte(' ')
	}
	return b.String()
}

// spansText is the spans as plain text.
func spansText(spans []span) string {
	var b strings.Builder
	for _, sp := range spans {
		b.WriteString(sp.text)
	}
	return b.String()
}
