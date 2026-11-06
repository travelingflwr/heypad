package main

// A made-up mailbox for screenshots (`heypad render --demo`), so pictures
// of heypad never show anyone's real mail.

import "time"

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
	{"Fieldnotes Weekly", "editor@fieldnotes.example", "#C9A2FF", "Issue 112: The quiet tools", "This week: why the best software disappears, a field guide to terminal typography...", 3 * time.Hour, 1, ""},
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
