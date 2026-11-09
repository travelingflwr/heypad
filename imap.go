package main

// IMAP accounts as heypad boxes. Each folder an account shows is a box whose
// kind is "acct:<name>:<mailbox>". Messages are grouped into threads by
// their References, and the Inbox gets HEY's two sections: New For You
// (threads with anything unseen) and Previously Seen.
//
// Threads get IDs above imapTopicBase, apart from HEY's. What a thread is
// (its account, folder, messages, and who a reply goes to) is kept in
// imap-threads.json in the state folder, so `heypad write` in another
// process can reply to it.

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"mime/quotedprintable"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message"
	"github.com/emersion/go-message/charset"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"
)

const (
	acctPrefix    = "acct:"
	imapTopicBase = int64(1) << 62
	imapWindow    = 300 // how many of a folder's newest messages to read
	imapSeenKeep  = 20  // how much of an Inbox's Previously Seen to show
	imapBoxKeep   = 60  // how many threads other folders show
)

func accountKind(name, mailbox string) string { return acctPrefix + name + ":" + mailbox }

func splitKind(kind string) (name, mailbox string, ok bool) {
	if !strings.HasPrefix(kind, acctPrefix) {
		return "", "", false
	}
	name, mailbox, ok = strings.Cut(strings.TrimPrefix(kind, acctPrefix), ":")
	return
}

func isIMAPTopic(id int64) bool { return id >= imapTopicBase }

// --- what each thread is ------------------------------------------------

type imapThread struct {
	Account     string   `json:"account"`
	Mailbox     string   `json:"mailbox"`
	UIDValidity uint32   `json:"uid_validity"`
	UIDs        []uint32 `json:"uids"`
	Subject     string   `json:"subject"`
	ReplyTo     string   `json:"reply_to"`   // the address a reply goes to
	ReplyFrom   string   `json:"reply_from"` // which of your addresses it was sent to
	MessageID   string   `json:"message_id"`
	References  []string `json:"references"`
}

var threads = struct {
	sync.Mutex
	m    map[int64]imapThread
	read time.Time // the file's modification time when last read
}{m: map[int64]imapThread{}}

func threadsPath() string { return filepath.Join(stateDir(), "imap-threads.json") }

func lookupThread(id int64) (imapThread, bool) {
	threads.Lock()
	defer threads.Unlock()
	if t, ok := threads.m[id]; ok {
		return t, true
	}
	// Another heypad (the viewer) may have listed it: read the file, if
	// it changed since the last look.
	st, err := os.Stat(threadsPath())
	if err != nil || !st.ModTime().After(threads.read) {
		return imapThread{}, false
	}
	threads.read = st.ModTime()
	if data, err := os.ReadFile(threadsPath()); err == nil {
		var m map[int64]imapThread
		if json.Unmarshal(data, &m) == nil {
			for k, v := range m {
				if _, ok := threads.m[k]; !ok {
					threads.m[k] = v
				}
			}
		}
	}
	t, ok := threads.m[id]
	return t, ok
}

func rememberThreads(found map[int64]imapThread) {
	threads.Lock()
	defer threads.Unlock()
	for k, v := range found {
		threads.m[k] = v
	}
	if len(threads.m) > 4000 {
		for k := range threads.m {
			if _, ok := found[k]; !ok {
				delete(threads.m, k)
			}
			if len(threads.m) <= 3000 {
				break
			}
		}
	}
	data, _ := json.Marshal(threads.m)
	os.MkdirAll(stateDir(), 0o700)
	tmp := threadsPath() + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		os.Rename(tmp, threadsPath())
	}
}

// --- connections --------------------------------------------------------

type imapSession struct {
	mu   sync.Mutex
	acct account
	c    *imapclient.Client
	sel  string
	// noStructure are folders where the server describes some message's
	// structure in a way the IMAP library can't read; their previews come
	// from the start of each message instead.
	noStructure map[string]bool
}

var sessions = struct {
	sync.Mutex
	m map[string]*imapSession
}{m: map[string]*imapSession{}}

func session(name string) (*imapSession, error) {
	a, ok := findAccount(name)
	if !ok {
		return nil, fmt.Errorf("no account called %s", name)
	}
	sessions.Lock()
	defer sessions.Unlock()
	s, ok := sessions.m[a.Name]
	if !ok {
		s = &imapSession{acct: a}
		sessions.m[a.Name] = s
	}
	return s, nil
}

func dialIMAP(a account, pw string) (*imapclient.Client, error) {
	c, err := imapclient.DialTLS(a.IMAP, &imapclient.Options{Dialer: &net.Dialer{Timeout: 15 * time.Second}})
	if err != nil {
		return nil, err
	}
	if err := c.Login(a.User, pw).Wait(); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func checkLogin(a account, pw string) error {
	c, err := dialIMAP(a, pw)
	if err != nil {
		return err
	}
	c.Logout().Wait()
	c.Close()
	return nil
}

// run calls fn with a signed-in connection. A dropped connection gets one
// fresh try; an answer from the server (NO, BAD) doesn't.
func (s *imapSession) run(fn func(c *imapclient.Client) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for attempt := 0; ; attempt++ {
		if s.c == nil {
			pw, err := s.acct.password()
			if err != nil {
				return err
			}
			if s.c, err = dialIMAP(s.acct, pw); err != nil {
				return err
			}
			s.sel = ""
		}
		err := fn(s.c)
		if err == nil {
			return nil
		}
		var serr *imap.Error
		if errors.As(err, &serr) || attempt > 0 {
			return err
		}
		s.c.Close()
		s.c = nil
	}
}

func (s *imapSession) selectBox(c *imapclient.Client, mailbox string, fresh bool) (*imap.SelectData, error) {
	if s.sel == mailbox && !fresh {
		return nil, nil
	}
	data, err := c.Select(mailbox, nil).Wait()
	if err != nil {
		s.sel = ""
		return nil, err
	}
	s.sel = mailbox
	return data, nil
}

// --- listing a folder ---------------------------------------------------

type imapMsg struct {
	uid  imap.UID
	env  *imap.Envelope
	date time.Time
	seen bool
	refs []string
	bs   imap.BodyStructure
}

func (m imapMsg) threadKey() string {
	switch {
	case len(m.refs) > 0:
		return m.refs[0]
	case len(m.env.InReplyTo) > 0:
		return m.env.InReplyTo[0]
	case m.env.MessageID != "":
		return m.env.MessageID
	}
	return fmt.Sprint("uid:", m.uid)
}

func topicID(acct, mailbox, key string) int64 {
	h := fnv.New64a()
	h.Write([]byte(acct + "\x00" + mailbox + "\x00" + key))
	return imapTopicBase + int64(h.Sum64()%uint64(imapTopicBase))
}

func refsFrom(raw []byte) []string {
	h, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(raw)))
	if err != nil {
		return nil
	}
	mh := mail.Header{Header: message.Header{Header: h}}
	l, _ := mh.MsgIDList("References")
	return l
}

func contactOf(addrs []imap.Address) Contact {
	if len(addrs) == 0 {
		return Contact{}
	}
	a := addrs[0]
	return Contact{Name: a.Name, Email: a.Addr(), AvatarColor: avatarColorFor(a.Addr())}
}

// avatarColorFor gives each sender a steady color, as HEY does.
func avatarColorFor(addr string) string {
	colors := []string{"#FFB86B", "#9DD3A8", "#7FB5FF", "#C9A2FF", "#FF8FA3", "#5ED6C9", "#F5E663", "#A8C5FF", "#FFC6E0", "#B8E986"}
	h := fnv.New32a()
	h.Write([]byte(strings.ToLower(addr)))
	return colors[h.Sum32()%uint32(len(colors))]
}

// yours is every address of a's: the first is what new mail is sent from.
func (a account) yours() []string {
	var out []string
	for _, s := range strings.Split(a.From, ",") {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (a account) isYours(addr string) bool {
	addr = strings.ToLower(addr)
	for _, y := range a.yours() {
		if y == addr {
			return true
		}
	}
	return strings.EqualFold(addr, a.User)
}

func fetchIMAPBox(kind string) ([]Posting, error) {
	name, mailbox, _ := splitKind(kind)
	s, err := session(name)
	if err != nil {
		return nil, err
	}
	a := s.acct
	var bufs []*imapclient.FetchMessageBuffer
	var validity uint32
	refsSec := &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, HeaderFields: []string{"References"}, Peek: true}
	list := func(structure bool) error {
		return s.run(func(c *imapclient.Client) error {
			data, err := s.selectBox(c, mailbox, true)
			if err != nil {
				return err
			}
			validity, bufs = data.UIDValidity, nil
			n := data.NumMessages
			if n == 0 {
				return nil
			}
			var seq imap.SeqSet
			seq.AddRange(max(1, n-imapWindow+1), n)
			opt := &imap.FetchOptions{
				UID: true, Flags: true, Envelope: true, InternalDate: true,
				BodySection: []*imap.FetchItemBodySection{refsSec},
			}
			if structure {
				opt.BodyStructure = &imap.FetchItemBodyStructure{}
			}
			bufs, err = c.Fetch(seq, opt).Collect()
			return err
		})
	}
	s.mu.Lock()
	structure := !s.noStructure[mailbox]
	s.mu.Unlock()
	err = list(structure)
	var serr *imap.Error
	if err != nil && structure && !errors.As(err, &serr) {
		// Most likely a body structure the library can't parse: list again
		// without them, and remember for this folder.
		s.mu.Lock()
		if s.noStructure == nil {
			s.noStructure = map[string]bool{}
		}
		s.noStructure[mailbox] = true
		s.mu.Unlock()
		err = list(false)
	}
	if err != nil {
		return nil, err
	}

	groups := map[string][]imapMsg{}
	for _, b := range bufs {
		if b.Envelope == nil {
			continue
		}
		m := imapMsg{uid: b.UID, env: b.Envelope, date: b.Envelope.Date, bs: b.BodyStructure}
		if m.date.IsZero() {
			m.date = b.InternalDate
		}
		for _, f := range b.Flags {
			if f == imap.FlagSeen {
				m.seen = true
			}
		}
		for _, sec := range b.BodySection {
			m.refs = refsFrom(sec.Bytes)
		}
		groups[m.threadKey()] = append(groups[m.threadKey()], m)
	}

	inbox := strings.EqualFold(mailbox, "INBOX")
	sent := strings.EqualFold(mailbox, a.Sent)
	type built struct {
		p      Posting
		latest imapMsg
	}
	var all []built
	found := map[int64]imapThread{}
	for key, msgs := range groups {
		sort.Slice(msgs, func(i, j int) bool { return msgs[i].date.Before(msgs[j].date) })
		latest := msgs[len(msgs)-1]
		unseen := false
		t := imapThread{Account: a.Name, Mailbox: mailbox, UIDValidity: validity, Subject: msgs[0].env.Subject}
		for _, m := range msgs {
			t.UIDs = append(t.UIDs, uint32(m.uid))
			if !m.seen {
				unseen = true
			}
		}
		// A reply goes to the latest message someone else sent, from the
		// address of yours they sent it to.
		reply := latest
		for i := len(msgs) - 1; i >= 0; i-- {
			if len(msgs[i].env.From) > 0 && !a.isYours(msgs[i].env.From[0].Addr()) {
				reply = msgs[i]
				break
			}
		}
		to := reply.env.ReplyTo
		if len(to) == 0 {
			to = reply.env.From
		}
		if sent {
			to = reply.env.To
		}
		if len(to) > 0 {
			t.ReplyTo = to[0].Addr()
		}
		for _, list := range [][]imap.Address{reply.env.To, reply.env.Cc} {
			for _, ad := range list {
				if t.ReplyFrom == "" && a.isYours(ad.Addr()) {
					t.ReplyFrom = ad.Addr()
				}
			}
		}
		t.MessageID = reply.env.MessageID
		t.References = append(append([]string{}, reply.refs...), reply.env.MessageID)
		id := topicID(a.Name, mailbox, key)
		found[id] = t

		who := latest.env.From
		if sent {
			who = latest.env.To
		}
		p := Posting{
			ID: id, TopicID: id, Subject: firstNonEmpty(t.Subject, latest.env.Subject),
			ActiveAt: latest.date, ObservedAt: latest.date, Creator: contactOf(who),
			Count: len(msgs), New: inbox && unseen, Seen: !unseen,
		}
		all = append(all, built{p, latest})
	}
	rememberThreads(found)

	sort.Slice(all, func(i, j int) bool {
		if all[i].p.New != all[j].p.New {
			return all[i].p.New
		}
		return all[i].p.ActiveAt.After(all[j].p.ActiveAt)
	})
	var keep []built
	seen := 0
	for _, b := range all {
		if inbox && !b.p.New {
			if seen >= imapSeenKeep {
				continue
			}
			seen++
		}
		if !inbox && len(keep) >= imapBoxKeep {
			break
		}
		keep = append(keep, b)
	}

	// Previews: the start of each thread's latest message.
	var latest []imapMsg
	for _, b := range keep {
		latest = append(latest, b.latest)
	}
	previews := s.previews(mailbox, latest)
	out := make([]Posting, len(keep))
	for i, b := range keep {
		b.p.Summary = previews[b.latest.uid]
		out[i] = b.p
	}
	writeCache(boxCache(kind), out)
	return out, nil
}

// textPart finds the part of a message to preview: plain text if there is
// one, else HTML.
func textPart(bs imap.BodyStructure) (path []int, part *imap.BodyStructureSinglePart) {
	var html []int
	var htmlPart *imap.BodyStructureSinglePart
	if bs == nil {
		return nil, nil
	}
	bs.Walk(func(p []int, b imap.BodyStructure) bool {
		sp, ok := b.(*imap.BodyStructureSinglePart)
		if !ok || part != nil {
			return part == nil
		}
		if d := sp.Disposition(); d != nil && strings.EqualFold(d.Value, "attachment") {
			return true
		}
		switch strings.ToLower(sp.MediaType()) {
		case "text/plain":
			path, part = append([]int{}, p...), sp
		case "text/html":
			if htmlPart == nil {
				html, htmlPart = append([]int{}, p...), sp
			}
		}
		return true
	})
	if part == nil {
		return html, htmlPart
	}
	if len(path) == 0 {
		path = []int{1} // a single-part message's body is part 1
	}
	return path, part
}

func (s *imapSession) previews(mailbox string, msgs []imapMsg) map[imap.UID]string {
	out := map[imap.UID]string{}
	parts := map[imap.UID]*imap.BodyStructureSinglePart{}
	byPath := map[string][]imap.UID{}
	paths := map[string][]int{}
	var whole []imap.UID // no structure: preview from the message's start
	for _, m := range msgs {
		if m.bs == nil {
			whole = append(whole, m.uid)
			continue
		}
		path, part := textPart(m.bs)
		if part == nil {
			continue
		}
		k := fmt.Sprint(path)
		parts[m.uid], paths[k] = part, path
		byPath[k] = append(byPath[k], m.uid)
	}
	s.run(func(c *imapclient.Client) error {
		if _, err := s.selectBox(c, mailbox, false); err != nil {
			return err
		}
		for k, uids := range byPath {
			sec := &imap.FetchItemBodySection{Part: paths[k], Peek: true, Partial: &imap.SectionPartial{Offset: 0, Size: 3000}}
			bufs, err := c.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{sec}}).Collect()
			if err != nil {
				return err
			}
			for _, b := range bufs {
				for _, bs := range b.BodySection {
					out[b.UID] = previewText(bs.Bytes, parts[b.UID])
				}
			}
		}
		if len(whole) > 0 {
			sec := &imap.FetchItemBodySection{Peek: true, Partial: &imap.SectionPartial{Offset: 0, Size: 16000}}
			bufs, err := c.Fetch(imap.UIDSetNum(whole...), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{sec}}).Collect()
			if err != nil {
				return err
			}
			for _, b := range bufs {
				for _, bs := range b.BodySection {
					_, sum := messageMarkdown(bs.Bytes)
					if len(sum) > 400 {
						sum = sum[:400]
					}
					out[b.UID] = sum
				}
			}
		}
		return nil
	})
	return out
}

// previewText decodes the start of a text part into a line of plain text.
func previewText(raw []byte, part *imap.BodyStructureSinglePart) string {
	var r io.Reader = bytes.NewReader(raw)
	switch strings.ToLower(part.Encoding) {
	case "base64":
		clean := strings.Map(func(c rune) rune {
			if c == '\r' || c == '\n' || c == ' ' {
				return -1
			}
			return c
		}, string(raw))
		clean = clean[:len(clean)/4*4]
		r = base64.NewDecoder(base64.StdEncoding, strings.NewReader(clean))
	case "quoted-printable":
		r = quotedprintable.NewReader(r)
	}
	if cs := part.Params["charset"]; cs != "" && !strings.EqualFold(cs, "utf-8") && !strings.EqualFold(cs, "us-ascii") {
		if cr, err := charset.Reader(cs, r); err == nil {
			r = cr
		}
	}
	data, _ := io.ReadAll(r) // a cut-off part ends in an error; keep what decoded
	text := string(data)
	if strings.EqualFold(part.MediaType(), "text/html") {
		text = htmlToText(text)
	}
	var lines []string
	for _, ln := range strings.Split(text, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, ">") {
			continue
		}
		lines = append(lines, ln)
	}
	return inline(strings.Join(lines, " "))
}

// --- reading a thread ---------------------------------------------------

func fetchIMAPThread(topic int64) ([]Entry, error) {
	t, ok := lookupThread(topic)
	if !ok {
		return nil, errors.New("heypad doesn't know this thread any more; refresh the box")
	}
	s, err := session(t.Account)
	if err != nil {
		return nil, err
	}
	var bufs []*imapclient.FetchMessageBuffer
	err = s.run(func(c *imapclient.Client) error {
		data, err := s.selectBox(c, t.Mailbox, true)
		if err != nil {
			return err
		}
		if data != nil && data.UIDValidity != t.UIDValidity {
			return errors.New("the folder changed on the server; refresh the box")
		}
		uids := make([]imap.UID, len(t.UIDs))
		for i, u := range t.UIDs {
			uids[i] = imap.UID(u)
		}
		bufs, err = c.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{
			UID: true, Envelope: true, InternalDate: true,
			BodySection: []*imap.FetchItemBodySection{{Peek: true}},
		}).Collect()
		return err
	})
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, b := range bufs {
		if b.Envelope == nil {
			continue
		}
		var raw []byte
		for _, bs := range b.BodySection {
			raw = bs.Bytes
		}
		md, summary := messageMarkdown(raw)
		when := b.Envelope.Date
		if when.IsZero() {
			when = b.InternalDate
		}
		out = append(out, Entry{
			ID: int64(b.UID), CreatedAt: when.UTC().Format("2006-01-02T15:04"),
			Creator: contactOf(b.Envelope.From), Body: md, Summary: summary,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].when().Before(out[j].when()) })
	writeCache(threadCache(topic), out)
	return out, nil
}

// messageMarkdown turns a raw message into Markdown for the reader: its
// HTML if it has some, else its plain text, with images inside the message
// as data URIs and its attachments listed at the end.
func messageMarkdown(raw []byte) (md, summary string) {
	mr, err := mail.CreateReader(bytes.NewReader(raw))
	if err != nil && !message.IsUnknownCharset(err) {
		return "*heypad couldn't read this message.*", ""
	}
	var plain, htmlBody string
	cids := map[string]string{}
	var attachments []string
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			if message.IsUnknownCharset(err) {
				continue
			}
			break
		}
		body, _ := io.ReadAll(io.LimitReader(p.Body, 15<<20))
		switch h := p.Header.(type) {
		case *mail.InlineHeader:
			ct, _, _ := h.ContentType()
			switch {
			case ct == "text/plain" && plain == "":
				plain = string(body)
			case ct == "text/html" && htmlBody == "":
				htmlBody = string(body)
			case strings.HasPrefix(ct, "image/"):
				if cid := strings.Trim(h.Get("Content-Id"), "<> "); cid != "" && len(body) < 6<<20 {
					cids[cid] = "data:" + ct + ";base64," + base64.StdEncoding.EncodeToString(body)
				}
			}
		case *mail.AttachmentHeader:
			ct, _, _ := h.ContentType()
			if cid := strings.Trim(h.Get("Content-Id"), "<> "); cid != "" && strings.HasPrefix(ct, "image/") && len(body) < 6<<20 {
				cids[cid] = "data:" + ct + ";base64," + base64.StdEncoding.EncodeToString(body)
			}
			name, _ := h.Filename()
			attachments = append(attachments, fmt.Sprintf("%s (%s)", firstNonEmpty(name, "attachment"), sizeText(len(body))))
		}
	}
	if htmlBody != "" {
		md = htmlToMarkdown(htmlBody, cids)
	} else {
		md = plainToMarkdown(plain)
	}
	for _, a := range attachments {
		md += "\n\n*Attachment: " + escapeMarkdown(a) + "*"
	}
	text := plain
	if text == "" {
		text = htmlToText(htmlBody)
	}
	return md, inline(strings.Join(strings.Fields(text), " "))
}

func sizeText(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}

// --- marking seen -------------------------------------------------------

func markIMAPSeen(ids []int64) error {
	type place struct{ acct, mailbox string }
	byPlace := map[place][]imap.UID{}
	for _, id := range ids {
		t, ok := lookupThread(id)
		if !ok {
			continue
		}
		k := place{t.Account, t.Mailbox}
		for _, u := range t.UIDs {
			byPlace[k] = append(byPlace[k], imap.UID(u))
		}
	}
	for k, uids := range byPlace {
		s, err := session(k.acct)
		if err != nil {
			return err
		}
		err = s.run(func(c *imapclient.Client) error {
			if _, err := s.selectBox(c, k.mailbox, false); err != nil {
				return err
			}
			return c.Store(imap.UIDSetNum(uids...), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagSeen}}, nil).Close()
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// trashIMAP moves threads to their account's trash folder.
func trashIMAP(ids []int64) error {
	type place struct{ acct, mailbox string }
	byPlace := map[place][]imap.UID{}
	for _, id := range ids {
		t, ok := lookupThread(id)
		if !ok {
			return errors.New("heypad doesn't know one of these threads any more; refresh the box")
		}
		k := place{t.Account, t.Mailbox}
		for _, u := range t.UIDs {
			byPlace[k] = append(byPlace[k], imap.UID(u))
		}
	}
	for k, uids := range byPlace {
		s, err := session(k.acct)
		if err != nil {
			return err
		}
		if strings.EqualFold(k.mailbox, s.acct.trash()) {
			continue // already there
		}
		err = s.run(func(c *imapclient.Client) error {
			if _, err := s.selectBox(c, k.mailbox, false); err != nil {
				return err
			}
			_, err := c.Move(imap.UIDSetNum(uids...), s.acct.trash()).Wait()
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// appendMessage saves msg in a folder, as sent mail or a draft.
func appendMessage(a account, mailbox string, msg []byte, flags []imap.Flag) error {
	s, err := session(a.Name)
	if err != nil {
		return err
	}
	return s.run(func(c *imapclient.Client) error {
		cmd := c.Append(mailbox, int64(len(msg)), &imap.AppendOptions{Flags: flags, Time: time.Now()})
		if _, err := cmd.Write(msg); err != nil {
			cmd.Close()
			return err
		}
		if err := cmd.Close(); err != nil {
			return err
		}
		_, err := cmd.Wait()
		return err
	})
}
