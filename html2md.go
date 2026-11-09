package main

// Turning mail from IMAP accounts into the Markdown the reader parses (see
// rich.go), so every account draws the same way. HTML mail is mostly layout
// tables and inline styles; this keeps the text, its emphasis, links,
// lists, quotes, code and images, keeps real data tables, and flattens the
// rest.

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

var (
	reBlankLines = regexp.MustCompile(`\n{3,}`)
	reMdSpecial  = regexp.MustCompile("[\\\\`*_\\[\\]<>|~#]")
	reHiddenCSS  = regexp.MustCompile(`(?i)display\s*:\s*none|mso-hide\s*:\s*all|visibility\s*:\s*hidden|max-height\s*:\s*0`)
	reWS         = regexp.MustCompile(`[ \t\r\n\f\x{00a0}]+`)
)

// escapeMarkdown escapes what the reader would read as Markdown, leaving
// URLs whole so they stay links.
func escapeMarkdown(s string) string {
	var b strings.Builder
	last := 0
	for _, m := range reBareURL.FindAllStringIndex(s, -1) {
		b.WriteString(reMdSpecial.ReplaceAllString(s[last:m[0]], `\$0`))
		b.WriteString(s[m[0]:m[1]])
		last = m[1]
	}
	b.WriteString(reMdSpecial.ReplaceAllString(s[last:], `\$0`))
	return b.String()
}

// plainToMarkdown keeps a plain-text message's lines (quotes included) and
// escapes the rest.
func plainToMarkdown(s string) string {
	var out []string
	for _, ln := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		prefix := ""
		for {
			t := strings.TrimLeft(ln, " ")
			if !strings.HasPrefix(t, ">") {
				break
			}
			prefix += "> "
			ln = strings.TrimPrefix(t[1:], " ")
		}
		if ln == "-- " || ln == "--" {
			out = append(out, prefix+"---")
			continue
		}
		out = append(out, prefix+escapeMarkdown(ln))
	}
	return strings.Join(out, "\n")
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

func hidden(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	switch n.DataAtom {
	case atom.Script, atom.Style, atom.Head, atom.Title, atom.Meta, atom.Noscript, atom.Template, atom.Svg, atom.Link:
		return true
	}
	if _, ok := attrOK(n, "hidden"); ok {
		return true
	}
	return reHiddenCSS.MatchString(attr(n, "style"))
}

func attrOK(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val, true
		}
	}
	return "", false
}

// htmlToText is an HTML message's text, a line per block, for previews.
func htmlToText(s string) string {
	doc, err := html.Parse(strings.NewReader(s))
	if err != nil {
		return ""
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if hidden(n) {
			return
		}
		if n.Type == html.TextNode {
			b.WriteString(reWS.ReplaceAllString(n.Data, " "))
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode && isBlock(n) {
			b.WriteString("\n")
		}
	}
	walk(doc)
	return b.String()
}

func isBlock(n *html.Node) bool {
	switch n.DataAtom {
	case atom.P, atom.Div, atom.Section, atom.Article, atom.Header, atom.Footer, atom.Main, atom.Aside, atom.Nav,
		atom.Center, atom.Table, atom.Tbody, atom.Thead, atom.Tfoot, atom.Tr, atom.Td, atom.Th, atom.Ul, atom.Ol, atom.Li,
		atom.Blockquote, atom.Pre, atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6, atom.Hr, atom.Br, atom.Dl, atom.Dt,
		atom.Dd, atom.Figure, atom.Figcaption, atom.Address, atom.Form, atom.Fieldset:
		return true
	}
	return false
}

type mdConv struct {
	cids  map[string]string
	depth int // list nesting
}

// htmlToMarkdown converts an HTML message. cids maps a Content-ID to the
// image it names, for <img src="cid:…">.
func htmlToMarkdown(s string, cids map[string]string) string {
	doc, err := html.Parse(strings.NewReader(s))
	if err != nil {
		return plainToMarkdown(s)
	}
	c := &mdConv{cids: cids}
	return tidy(c.children(doc))
}

// tidy trims each line's end (keeping hard breaks) and folds blank lines.
func tidy(md string) string {
	lines := strings.Split(md, "\n")
	for i, ln := range lines {
		hard := strings.HasSuffix(ln, "  ") && strings.TrimSpace(ln) != ""
		ln = strings.TrimRight(ln, " \t")
		if strings.TrimSpace(strings.Trim(ln, "> ")) == "" {
			ln = strings.TrimRight(ln, "> ")
		} else if hard {
			ln += "  "
		}
		lines[i] = ln
	}
	return strings.TrimSpace(reBlankLines.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

func (c *mdConv) children(n *html.Node) string {
	var b strings.Builder
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		b.WriteString(c.node(ch))
	}
	return b.String()
}

// wrap puts marker around s's text, keeping its outer spaces outside, and
// leaves s alone when it spans blocks or is empty.
func wrap(s, open, close string) string {
	t := strings.TrimSpace(s)
	if t == "" || strings.Contains(t, "\n\n") || strings.Contains(t, "![") {
		return s
	}
	lead := s[:len(s)-len(strings.TrimLeft(s, " \n"))]
	trail := s[len(strings.TrimRight(s, " \n")):]
	return lead + open + t + close + trail
}

func asBlock(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return "\n\n" + strings.TrimSpace(s) + "\n\n"
}

func (c *mdConv) node(n *html.Node) string {
	if hidden(n) {
		return ""
	}
	switch n.Type {
	case html.TextNode:
		return escapeMarkdown(reWS.ReplaceAllString(n.Data, " "))
	case html.DocumentNode:
		return c.children(n)
	case html.ElementNode:
	default:
		return ""
	}
	switch n.DataAtom {
	case atom.Br:
		return "  \n"
	case atom.Hr:
		return "\n\n---\n\n"
	case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
		level := int(n.Data[1] - '0')
		text := strings.Join(strings.Fields(strings.ReplaceAll(c.children(n), "  \n", " ")), " ")
		if text == "" {
			return ""
		}
		return "\n\n" + strings.Repeat("#", level) + " " + text + "\n\n"
	case atom.Strong, atom.B:
		return wrap(c.children(n), "**", "**")
	case atom.Em, atom.I, atom.Cite:
		return wrap(c.children(n), "*", "*")
	case atom.S, atom.Strike, atom.Del:
		return wrap(c.children(n), "~~", "~~")
	case atom.Code, atom.Tt, atom.Kbd, atom.Samp:
		if t := textOf(n); strings.TrimSpace(t) != "" {
			return "`" + strings.ReplaceAll(t, "`", "'") + "`"
		}
		return ""
	case atom.Pre:
		return "\n\n```\n" + strings.Trim(textOf(n), "\n") + "\n```\n\n"
	case atom.A:
		inner := c.children(n)
		href := strings.TrimSpace(attr(n, "href"))
		if href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(strings.ToLower(href), "javascript:") {
			return inner
		}
		t := strings.TrimSpace(inner)
		if t == "" || strings.Contains(t, "\n\n") {
			return inner
		}
		lead := inner[:len(inner)-len(strings.TrimLeft(inner, " \n"))]
		trail := inner[len(strings.TrimRight(inner, " \n")):]
		return lead + "[" + t + "](" + mdURL(href) + ")" + trail
	case atom.Img:
		return c.img(n)
	case atom.Ul, atom.Ol:
		return c.list(n)
	case atom.Blockquote:
		inner := tidy(c.children(n))
		if inner == "" {
			return ""
		}
		lines := strings.Split(inner, "\n")
		for i, ln := range lines {
			lines[i] = "> " + ln
		}
		return "\n\n" + strings.Join(lines, "\n") + "\n\n"
	case atom.Table:
		return c.table(n)
	}
	if isBlock(n) {
		return asBlock(c.children(n))
	}
	return c.children(n)
}

// textOf is the raw text under n.
func textOf(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		if n.Type == html.ElementNode && n.DataAtom == atom.Br {
			b.WriteString("\n")
		}
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			walk(ch)
		}
	}
	walk(n)
	return b.String()
}

func mdURL(u string) string {
	u = strings.ReplaceAll(u, " ", "%20")
	return strings.NewReplacer("(", "%28", ")", "%29").Replace(u)
}

func (c *mdConv) img(n *html.Node) string {
	src := strings.TrimSpace(attr(n, "src"))
	if src == "" {
		return ""
	}
	// Tracking pixels say so in their size.
	for _, k := range []string{"width", "height"} {
		if v, err := strconv.Atoi(strings.TrimSuffix(attr(n, k), "px")); err == nil && v <= 2 {
			return ""
		}
	}
	if strings.HasPrefix(strings.ToLower(src), "cid:") {
		cid, _ := url.PathUnescape(src[4:])
		data, ok := c.cids[cid]
		if !ok {
			return ""
		}
		src = data
	}
	alt := strings.Join(strings.Fields(attr(n, "alt")), " ")
	return fmt.Sprintf("![%s](%s)", reMdSpecial.ReplaceAllString(alt, ""), mdURL(src))
}

func (c *mdConv) list(n *html.Node) string {
	c.depth++
	defer func() { c.depth-- }()
	var b strings.Builder
	num := 1
	if v, err := strconv.Atoi(attr(n, "start")); err == nil {
		num = v
	}
	indent := strings.Repeat("  ", c.depth-1)
	for li := n.FirstChild; li != nil; li = li.NextSibling {
		if li.Type != html.ElementNode || li.DataAtom != atom.Li || hidden(li) {
			continue
		}
		marker := "- "
		if n.DataAtom == atom.Ol {
			marker = fmt.Sprintf("%d. ", num)
			num++
		}
		text := tidy(c.children(li))
		if text == "" {
			continue
		}
		// The item's own text stays on its line; a nested list keeps its
		// own indented lines.
		lines := strings.Split(text, "\n")
		var first []string
		rest := lines
		for len(rest) > 0 && !reListItem.MatchString(strings.TrimSpace(rest[0])) {
			if t := strings.TrimSpace(rest[0]); t != "" {
				first = append(first, t)
			}
			rest = rest[1:]
		}
		b.WriteString("\n" + indent + marker + strings.Join(first, " "))
		for _, ln := range rest {
			if strings.TrimSpace(ln) != "" {
				b.WriteString("\n" + ln)
			}
		}
	}
	if c.depth > 1 {
		return b.String() + "\n"
	}
	return "\n\n" + strings.TrimLeft(b.String(), "\n") + "\n\n"
}

// table keeps a real data table as a Markdown table, and turns a layout
// table (most tables in mail) into its cells' contents, one after another.
func (c *mdConv) table(n *html.Node) string {
	var rows [][]*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			if ch.Type != html.ElementNode || hidden(ch) {
				continue
			}
			switch ch.DataAtom {
			case atom.Thead, atom.Tbody, atom.Tfoot:
				walk(ch)
			case atom.Tr:
				var cells []*html.Node
				for td := ch.FirstChild; td != nil; td = td.NextSibling {
					if td.Type == html.ElementNode && (td.DataAtom == atom.Td || td.DataAtom == atom.Th) && !hidden(td) {
						cells = append(cells, td)
					}
				}
				rows = append(rows, cells)
			}
		}
	}
	walk(n)
	if isDataTable(rows) {
		var b strings.Builder
		b.WriteString("\n\n")
		for ri, row := range rows {
			b.WriteString("|")
			for _, td := range row {
				cell := strings.Join(strings.Fields(strings.ReplaceAll(c.children(td), "  \n", " ")), " ")
				b.WriteString(" " + cell + " |")
			}
			b.WriteString("\n")
			if ri == 0 {
				b.WriteString("|" + strings.Repeat("---|", len(row)) + "\n")
			}
		}
		// Markdown tables always have a header row; without <th> the first
		// row stands in, which suits most tables in mail.
		return b.String() + "\n"
	}
	var b strings.Builder
	for _, row := range rows {
		for _, td := range row {
			b.WriteString(asBlock(c.children(td)))
		}
	}
	return b.String()
}

// isDataTable: at least two rows of two or more cells, the same count in
// each row, and nothing in a cell that only layout tables hold.
func isDataTable(rows [][]*html.Node) bool {
	if len(rows) < 2 {
		return false
	}
	cols := len(rows[0])
	if cols < 2 || cols > 8 {
		return false
	}
	for _, r := range rows {
		if len(r) != cols {
			return false
		}
		for _, td := range r {
			if hasLayout(td) {
				return false
			}
		}
	}
	return true
}

func hasLayout(n *html.Node) bool {
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		if ch.Type == html.ElementNode {
			switch ch.DataAtom {
			case atom.Table, atom.Img, atom.P, atom.Div, atom.Ul, atom.Ol, atom.H1, atom.H2, atom.H3, atom.Blockquote:
				return true
			}
			if hasLayout(ch) {
				return true
			}
		}
	}
	return false
}
