package main

import (
	"strings"
	"testing"
)

func TestHTMLToMarkdown(t *testing.T) {
	in := `<html><head><style>p{color:red}</style><title>x</title></head><body>
<div style="display:none">Preheader you shouldn't see</div>
<table><tr><td><table><tr><td>
<h1>Spring <b>sale</b></h1>
<p>Hello <strong>Amy</strong>, see <a href="https://shop.example/a_b?x=1">our <em>new</em> things</a>.<br>Second line.</p>
<img src="https://img.example/pixel.gif" width="1" height="1">
<img src="cid:logo@x" alt="Logo">
<ul><li>One</li><li>Two<ul><li>Nested</li></ul></li></ul>
<ol start="3"><li>Third</li></ol>
<blockquote><p>Quoted <i>words</i></p></blockquote>
<pre>  code  here</pre>
<table><tr><th>Plan</th><th>Price</th></tr><tr><td>Basic</td><td>$5</td></tr></table>
<p>Snake_case and 2*3 stay literal.</p>
</td></tr></table></td></tr></table></body></html>`
	md := htmlToMarkdown(in, map[string]string{"logo@x": "data:image/png;base64,AAAA"})
	for _, want := range []string{
		"# Spring **sale**",
		"Hello **Amy**, see [our *new* things](https://shop.example/a_b?x=1).  \nSecond line.",
		"![Logo](data:image/png;base64,AAAA)",
		"- One\n- Two\n  - Nested",
		"3. Third",
		"> Quoted *words*",
		"```\n  code  here\n```",
		"| Plan | Price |\n|---|---|\n| Basic | $5 |",
		`Snake\_case and 2\*3 stay literal.`,
	} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in:\n%s", want, md)
		}
	}
	for _, bad := range []string{"Preheader", "pixel.gif", "color:red"} {
		if strings.Contains(md, bad) {
			t.Errorf("%q should be gone:\n%s", bad, md)
		}
	}
	// And it reads back as the right blocks.
	got := kinds(parseRich(md))
	want := []blockKind{bkHeading, bkPara, bkImage, bkItem, bkItem, bkItem, bkItem, bkPara, bkCode, bkTable, bkPara}
	if strings.Join(kindNames(got), " ") != strings.Join(kindNames(want), " ") {
		t.Errorf("blocks\n got %v\nwant %v\n%s", kindNames(got), kindNames(want), md)
	}
}

func kindNames(ks []blockKind) []string {
	names := []string{"para", "heading", "item", "code", "rule", "table", "image"}
	var out []string
	for _, k := range ks {
		out = append(out, names[k])
	}
	return out
}

const testMessage = "From: Rosa Delgado <rosa@larkspur.example>\r\n" +
	"To: you@example.com\r\n" +
	"Subject: =?utf-8?q?Trail_map_=E2=80=94_Saturday?=\r\n" +
	"Message-ID: <abc@larkspur.example>\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=outer\r\n" +
	"\r\n" +
	"--outer\r\n" +
	"Content-Type: multipart/related; boundary=rel\r\n" +
	"\r\n" +
	"--rel\r\n" +
	"Content-Type: multipart/alternative; boundary=alt\r\n" +
	"\r\n" +
	"--alt\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"Here's the map. See you at 8:30!\r\n" +
	"--alt\r\n" +
	"Content-Type: text/html; charset=iso-8859-1\r\n" +
	"Content-Transfer-Encoding: quoted-printable\r\n" +
	"\r\n" +
	"<p>Here's the <b>map</b>. Caf=E9 at 8:30!</p><img src=3D\"cid:map1\">\r\n" +
	"--alt--\r\n" +
	"--rel\r\n" +
	"Content-Type: image/png\r\n" +
	"Content-ID: <map1>\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"iVBORw0KGgo=\r\n" +
	"--rel--\r\n" +
	"--outer\r\n" +
	"Content-Type: application/pdf\r\n" +
	"Content-Disposition: attachment; filename=\"permit.pdf\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"JVBERi0xLjQK\r\n" +
	"--outer--\r\n"

func TestMessageMarkdown(t *testing.T) {
	md, summary := messageMarkdown([]byte(testMessage))
	for _, want := range []string{"Here's the **map**. Café at 8:30!", "![](data:image/png;base64,", "*Attachment: permit.pdf (9 bytes)*"} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in:\n%s", want, md)
		}
	}
	if summary != "Here's the map. See you at 8:30!" {
		t.Errorf("summary %q", summary)
	}
}

func TestPlainToMarkdown(t *testing.T) {
	md := plainToMarkdown("Hi *all*,\nsee https://x.example/a_b_c\n> quoted line\n-- \nSam")
	for _, want := range []string{`Hi \*all\*,`, "https://x.example/a_b_c", "> quoted line", "---"} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in:\n%s", want, md)
		}
	}
}

func TestThreadKeyAndTopics(t *testing.T) {
	if !isIMAPTopic(topicID("iCloud", "INBOX", "abc")) || isIMAPTopic(123456789) {
		t.Fatal("topic ranges overlap")
	}
	if topicID("iCloud", "INBOX", "a") == topicID("iCloud", "Archive", "a") {
		t.Fatal("same key in two folders should be two threads")
	}
	name, mb, ok := splitKind(accountKind("iCloud", "Sent Messages"))
	if !ok || name != "iCloud" || mb != "Sent Messages" {
		t.Fatalf("splitKind: %q %q %v", name, mb, ok)
	}
}

func TestSafeURL(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://example.com/a?b=c":            true,
		"http://example.com":                   true,
		"mailto:someone@example.com":           true,
		"HTTPS://EXAMPLE.COM":                  true,
		"file:///Applications/Calculator.app":  false,
		"-aCalculator":                         false,
		"-a Terminal":                          false,
		"/Applications/Calculator.app":         false,
		"x-apple.systempreferences:General":    false,
		"smb://files.example/share":            false,
		"javascript:alert(1)":                  false,
		"https:///nohost":                      false,
		"https://example.com/\x1b]2;title\x07": false,
		"":                                     false,
	} {
		if got := safeURL(raw); got != want {
			t.Errorf("safeURL(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestTermSafe(t *testing.T) {
	if got := termSafe("Lunch?\x07\x1b]52;c;AAAA\x07 \u009b31m café"); got != "Lunch?]52;c;AAAA 31m café" {
		t.Errorf("termSafe: %q", got)
	}
}
