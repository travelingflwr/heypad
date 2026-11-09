package main

import (
	"reflect"
	"testing"
)

func TestParseInline(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []span
	}{
		{"plain text", []span{{text: "plain text"}}},
		{"a **bold** and *italic* word", []span{{text: "a "}, {text: "bold", style: stBold}, {text: " and "}, {text: "italic", style: stItalic}, {text: " word"}}},
		{"***both***", []span{{text: "both", style: stBold | stItalic}}},
		{"snake_case_name stays", []span{{text: "snake_case_name stays"}}},
		{"use `go test ./...` now", []span{{text: "use "}, {text: "go test ./...", style: stCode}, {text: " now"}}},
		{"~~gone~~ here", []span{{text: "gone", style: stStrike}, {text: " here"}}},
		{"see [the docs](https://example.com/a_(b)) please", []span{{text: "see "}, {text: "the docs", link: "https://example.com/a_(b)"}, {text: " please"}}},
		{"[**bold link**](https://x.example)", []span{{text: "bold link", style: stBold, link: "https://x.example"}}},
		{"visit https://example.com/page.", []span{{text: "visit "}, {text: "https://example.com/page", link: "https://example.com/page"}, {text: "."}}},
		{"<https://auto.example>", []span{{text: "https://auto.example", link: "https://auto.example"}}},
		{"Tom &amp; Jerry", []span{{text: "Tom & Jerry"}}},
		{"an ![logo](https://img.example/l.png) inline", []span{{text: "an "}, {text: "logo", img: "https://img.example/l.png"}, {text: " inline"}}},
		{"[![banner](https://img.example/b.png)](https://shop.example)", []span{{text: "banner", img: "https://img.example/b.png", link: "https://shop.example"}}},
		{"2 * 3 * 4", []span{{text: "2 * 3 * 4"}}},
		{"line one<br>line two", []span{{text: "line one\nline two"}}},
		{"<span>tags</span> go", []span{{text: "tags go"}}},
		{"\\*not italic\\*", []span{{text: "*not italic*"}}},
	} {
		if got := parseInline(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseInline(%q)\n got %#v\nwant %#v", c.in, got, c.want)
		}
	}
}

func kinds(bs []block) []blockKind {
	var out []blockKind
	for _, b := range bs {
		out = append(out, b.kind)
	}
	return out
}

func TestParseRichBlocks(t *testing.T) {
	md := "# Title\n\nFirst line\nsame paragraph  \nhard break\n\n" +
		"- one\n- two\n  - nested\n1. first\n2. second\n\n" +
		"> quoted **text**\n\n---\n\n" +
		"```\nfunc main() {\n    fmt.Println(\"hi\")\n}\n```\n\n" +
		"| Plan | Price |\n|---|---|\n| Basic | $5 |\n| Pro | $9 |\n\n" +
		"![hero](https://img.example/h.png)\n"
	bs := parseRich(md)
	want := []blockKind{bkHeading, bkPara, bkItem, bkItem, bkItem, bkItem, bkItem, bkPara, bkRule, bkCode, bkTable, bkImage}
	if got := kinds(bs); !reflect.DeepEqual(got, want) {
		t.Fatalf("kinds\n got %v\nwant %v", got, want)
	}
	if bs[0].level != 1 || spansText(bs[0].spans) != "Title" {
		t.Errorf("heading: %+v", bs[0])
	}
	if got := spansText(bs[1].spans); got != "First line same paragraph\nhard break" {
		t.Errorf("paragraph text %q", got)
	}
	if bs[4].level != 1 || bs[4].marker != "◦" {
		t.Errorf("nested item: %+v", bs[4])
	}
	if bs[5].marker != "1." || bs[6].marker != "2." {
		t.Errorf("numbers: %q %q", bs[5].marker, bs[6].marker)
	}
	if bs[7].quote != 1 || bs[7].spans[1].style != stBold {
		t.Errorf("quote: %+v", bs[7])
	}
	if !reflect.DeepEqual(bs[9].code, []string{"func main() {", "    fmt.Println(\"hi\")", "}"}) {
		t.Errorf("code: %q", bs[9].code)
	}
	if tb := bs[10]; !tb.head || len(tb.rows) != 3 || len(tb.rows[0]) != 2 || spansText(tb.rows[2][1]) != "$9" {
		t.Errorf("table: %+v", tb)
	}
	if bs[11].src != "https://img.example/h.png" || bs[11].alt != "hero" {
		t.Errorf("image: %+v", bs[11])
	}
}

func TestLayoutTableBecomesParagraphs(t *testing.T) {
	// Marketing mail lays out with tables; one real column is just text.
	bs := parseRich("| | Hello there | |\n|---|---|---|\n| | ![x](https://i.example/x.png) | |\n| | Bye | |")
	if got := kinds(bs); !reflect.DeepEqual(got, []blockKind{bkPara, bkImage, bkPara}) {
		t.Fatalf("kinds %v", got)
	}
}

func TestImageSplitsParagraph(t *testing.T) {
	bs := parseRich("Before ![a](https://i.example/a.png) after")
	if got := kinds(bs); !reflect.DeepEqual(got, []blockKind{bkPara, bkImage, bkPara}) {
		t.Fatalf("kinds %v", got)
	}
}
