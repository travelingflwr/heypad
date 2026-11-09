package main

// A made-up mailbox for screenshots (`heypad render --demo`), so pictures
// of heypad never show anyone's real mail.

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"math"
	"time"
)

type demoMail struct {
	name, email, color, subject, summary string
	ago                                  time.Duration
	count                                int
	body                                 string
}

var demoMails = []demoMail{
	{"Rosa Delgado", "rosa@larkspur.studio", "#FFB86B", "Saturday hike — trail change", "Heads up: the north loop is closed for repairs, so we'll start from the lake lot instead...", 12 * time.Minute, 3,
		"Heads up: the north loop is closed for repairs, so we'll start from the lake lot instead. Same time, 8:30.\n\nI'll bring the thermos and the good snacks. Can someone grab the trail map from the ranger station on the way?\n\n> Are we still on for Saturday? I can drive if anyone needs a ride.\n\nYes! And thank you, Theo — Priya and I will take you up on that.\n\nRosa"},
	{"Paper Lantern Books", "hello@paperlantern.example", "#9DD3A8", "Your order has shipped", "Good news! The Overstory and two more are on their way. Track your package...", 47 * time.Minute, 1, ""},
	{"Theo Park", "theo@parkandco.example", "#7FB5FF", "Re: Kitchen remodel quotes", "I compared all three. The second one is cheaper but their timeline worries me...", 2 * time.Hour, 5, ""},
	{"Fieldnotes Weekly", "editor@fieldnotes.example", "#C9A2FF", "Issue 112: The quiet tools", "This week: why the best software disappears, a field guide to terminal typography...", 3 * time.Hour, 1, demoNewsletter},
	{"Priya Raman", "priya@raman.example", "#FF8FA3", "Photos from the reunion", "Finally sorted through them all! Uploaded the best 80 to the shared album...", 26 * time.Hour, 2, ""},
	{"Northwind Air", "trips@northwind.example", "#5ED6C9", "Check in for your flight to Lisbon", "Your flight NW 2207 departs tomorrow at 9:40 AM. Check in now to choose your seat...", 30 * time.Hour, 1, ""},
	{"Sam Okafor", "sam@okafor.example", "#F5E663", "Garden club: seed swap list", "Attached is the list so far. I have way too many tomato varieties, please take some...", 50 * time.Hour, 4, ""},
	{"City Library", "notices@library.example", "#A8C5FF", "A hold is ready for pickup", "Your hold on \"Braiding Sweetgrass\" is ready at the Central branch until Nov 13...", 4 * 24 * time.Hour, 1, ""},
	{"Mika Tanaka", "mika@tanaka.example", "#FFC6E0", "Lunch next week?", "It's been way too long. I'm free Tuesday or Thursday, anywhere near the office works...", 9 * 24 * time.Hour, 2, ""},
	{"Juniper Energy", "billing@juniper.example", "#B8E986", "Your November statement", "Your statement is ready. You used 12% less energy than last November...", 11 * 24 * time.Hour, 1, ""},
}

func demoPosts(now time.Time) []Posting {
	var out []Posting
	for i, m := range demoMails {
		out = append(out, Posting{
			ID: int64(1000 + i), TopicID: int64(2000 + i), Subject: m.subject, Summary: m.summary,
			ActiveAt: now.Add(-m.ago), Count: m.count,
			Creator: Contact{Name: m.name, Email: m.email, AvatarColor: m.color},
			New:     i < 3,
		})
	}
	return out
}

func demoThread(p Posting) []Entry {
	i := int(p.ID - 1000)
	m := demoMails[i]
	body := m.body
	if body == "" {
		body = m.summary
	}
	t := p.ActiveAt.UTC()
	reply := Entry{ID: 1, CreatedAt: t.Add(-3 * time.Hour).Format("2006-01-02T15:04"),
		Creator: Contact{Name: "Theo Park", Email: "theo@parkandco.example", AvatarColor: "#7FB5FF"},
		Body:    "Are we still on for Saturday? I can drive if anyone needs a ride."}
	last := Entry{ID: 2, CreatedAt: t.Format("2006-01-02T15:04"), Creator: p.Creator, Body: body}
	if i == 0 {
		return []Entry{reply, last}
	}
	return []Entry{last}
}

// demoTriage is a made-up triage of the demo mailbox's new threads.
func demoTriage() map[int64]TriageItem {
	return map[int64]TriageItem{
		1000: {"needs_you", "Confirm you'll grab the trail map"},
		1001: {"skip", "Shipping notice"},
		1002: {"needs_you", "Pick a contractor this week"},
	}
}

// demoNewsletter shows off the reader: headings, styles, links, lists, a
// quote, code, a table and an image.
var demoNewsletter = "# The quiet tools\n\n" +
	"![A desert ridge at dusk](" + demoImage() + ")\n\n" +
	"The best software **disappears** while you use it. This week we look at tools that stay *out of the way*, " +
	"and a few that get it ~~wrong~~ almost right. Read the [full essay](https://fieldnotes.example/112) online.\n\n" +
	"## In this issue\n\n" +
	"1. Terminal typography, explained\n" +
	"2. Why panes beat windows\n" +
	"   - and when they don't\n" +
	"3. A reader question\n\n" +
	"> Tools should feel like a well-worn trail: you notice them only when they're gone.\n\n" +
	"### Try it\n\n" +
	"Run `fieldnotes fetch --issue 112` or paste this into your shell:\n\n" +
	"```\nfor issue in 110 111 112; do\n    fieldnotes fetch --issue $issue\ndone\n```\n\n" +
	"| Tool | Platform | Price |\n|---|---|---|\n| Trailhead | Mac, iPad | Free |\n| Ridgeline | Linux | $12 |\n| Basecamp Notes | Everywhere | $5/mo |\n\n" +
	"---\n\n" +
	"Thanks for reading. Reply and tell us about *your* quiet tools.\n\n— The Fieldnotes team"

// demoImage is a made-up picture (a sky fading over two ridges) as a data
// URI, so screenshots need no network.
func demoImage() string {
	const w, h = 640, 240
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	lerp := func(a, b uint8, t float64) uint8 { return uint8(float64(a) + (float64(b)-float64(a))*t) }
	for y := 0; y < h; y++ {
		t := float64(y) / h
		sky := color.RGBA{lerp(70, 245, t), lerp(60, 150, t), lerp(140, 110, t), 255}
		for x := 0; x < w; x++ {
			far := 150 + 30*math.Sin(float64(x)/70) + 12*math.Sin(float64(x)/23)
			near := 190 + 22*math.Sin(float64(x)/110+1) + 8*math.Sin(float64(x)/31)
			c := sky
			if float64(y) > near {
				c = color.RGBA{38, 30, 52, 255}
			} else if float64(y) > far {
				c = color.RGBA{92, 62, 98, 255}
			}
			img.SetRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}
