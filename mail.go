package main

// Where each box's mail comes from: HEY (the hey CLI) or an IMAP account.
// The viewer calls these instead of HEY's functions directly.

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-message/mail"
)

// accountBoxes are the IMAP accounts' folders, as boxes after HEY's. An
// account's Inbox is titled with the account's name.
func accountBoxes() []box {
	var out []box
	for _, a := range loadAccounts() {
		for _, f := range a.Folders {
			title := a.Name + " · " + f.Title
			if strings.EqualFold(f.Mailbox, "INBOX") {
				title = a.Name
			}
			out = append(out, box{accountKind(a.Name, f.Mailbox), title})
		}
	}
	return out
}

// isInbox is a box with HEY's two sections: HEY's Imbox, or an account's
// Inbox.
func isInbox(kind string) bool {
	if kind == "imbox" {
		return true
	}
	_, mailbox, ok := splitKind(kind)
	return ok && strings.EqualFold(mailbox, "INBOX")
}

// boxAccount is the account a box belongs to, or "" for HEY.
func boxAccount(kind string) string {
	name, _, _ := splitKind(kind)
	return name
}

// windowTitle is the terminal title for a box.
func windowTitle(b box) string {
	if strings.HasPrefix(b.kind, acctPrefix) {
		return b.title
	}
	return "HEY · " + b.title
}

func loadBox(kind string, hint int) ([]Posting, error) {
	if _, _, ok := splitKind(kind); ok {
		return fetchIMAPBox(kind)
	}
	if kind == "imbox" {
		return loadImbox(hint)
	}
	return fetchBox(kind, hint)
}

// loadImbox is the unified Imbox: HEY's, with every account's Inbox merged
// in. New For You is everything unread from all of them, newest first;
// Previously Seen is the newest of what's been seen. If one source fails,
// the others still show; only all of them failing is an error.
func loadImbox(hint int) ([]Posting, error) {
	accts := loadAccounts()
	type got struct {
		posts []Posting
		err   error
	}
	results := make([]got, 1+len(accts))
	var wg sync.WaitGroup
	wg.Add(len(results))
	go func() {
		defer wg.Done()
		p, err := fetchBox("imbox", hint)
		results[0] = got{p, err}
	}()
	for i, a := range accts {
		go func(i int, a account) {
			defer wg.Done()
			p, err := fetchIMAPBox(accountKind(a.Name, "INBOX"))
			results[i+1] = got{p, err}
		}(i, a)
	}
	wg.Wait()
	var fresh, seen []Posting
	var errs []string
	for i, r := range results {
		if r.err != nil {
			name := "HEY"
			if i > 0 {
				name = accts[i-1].Name
			}
			errs = append(errs, name+": "+r.err.Error())
			continue
		}
		for _, p := range r.posts {
			if p.New {
				fresh = append(fresh, p)
			} else {
				seen = append(seen, p)
			}
		}
	}
	if len(errs) == len(results) {
		return nil, errors.New(strings.Join(errs, "; "))
	}
	newest := func(ps []Posting) {
		sort.SliceStable(ps, func(i, j int) bool { return ps[i].ObservedAt.After(ps[j].ObservedAt) })
	}
	newest(fresh)
	newest(seen)
	if len(seen) > seenToKeep {
		seen = seen[:seenToKeep]
	}
	out := append(fresh, seen...)
	writeCache(boxCache("imbox"), out)
	return out, nil
}

// postAccount is the account a thread came from, or "" for HEY.
func postAccount(p Posting) string {
	if !isIMAPTopic(p.TopicID) {
		return ""
	}
	if t, ok := lookupThread(p.TopicID); ok {
		return t.Account
	}
	return "Mail"
}

// heyOnly is posts without other accounts' threads, for what only HEY can
// do (triage reads threads with the hey CLI).
func heyOnly(posts []Posting) []Posting {
	var out []Posting
	for _, p := range posts {
		if !isIMAPTopic(p.TopicID) {
			out = append(out, p)
		}
	}
	return out
}

func loadThread(topic int64) ([]Entry, error) {
	if isIMAPTopic(topic) {
		return fetchIMAPThread(topic)
	}
	return fetchThread(topic)
}

func setSeen(p Posting) error {
	if isIMAPTopic(p.TopicID) {
		return markIMAPSeen([]int64{p.TopicID})
	}
	return markSeen(p)
}

func setSeenIDs(ids []int64) error {
	var hey, other []int64
	for _, id := range ids {
		if isIMAPTopic(id) {
			other = append(other, id)
		} else {
			hey = append(hey, id)
		}
	}
	if len(other) > 0 {
		if err := markIMAPSeen(other); err != nil {
			return err
		}
	}
	if len(hey) > 0 {
		return markSeenIDs(hey)
	}
	return nil
}

// trashIDs moves threads to the trash: HEY's with `hey trash`, other
// accounts' to their trash folder.
func trashIDs(ids []int64) error {
	var hey, other []int64
	for _, id := range ids {
		if isIMAPTopic(id) {
			other = append(other, id)
		} else {
			hey = append(hey, id)
		}
	}
	if len(other) > 0 {
		if err := trashIMAP(other); err != nil {
			return err
		}
	}
	for len(hey) > 0 {
		n := min(50, len(hey))
		args := []string{"trash"}
		for _, id := range hey[:n] {
			args = append(args, fmt.Sprint(id))
		}
		if _, err := heyJSON(args...); err != nil {
			return err
		}
		hey = hey[n:]
	}
	return nil
}

// sendFor sends (or saves as a draft) a reply to thread reply, or a new
// message from account acct ("" for HEY).
func sendFor(acct, reply, to, subject, body string, draft bool) error {
	if reply != "" {
		var id int64
		fmt.Sscan(reply, &id)
		if isIMAPTopic(id) {
			t, ok := lookupThread(id)
			if !ok {
				return errors.New("heypad doesn't know this thread any more")
			}
			a, ok := findAccount(t.Account)
			if !ok {
				return fmt.Errorf("no account called %s", t.Account)
			}
			subj := t.Subject
			if !strings.HasPrefix(strings.ToLower(subj), "re:") {
				subj = "Re: " + subj
			}
			return sendIMAP(a, firstNonEmpty(t.ReplyFrom, a.yours()[0]), t.ReplyTo, subj, body, t.MessageID, t.References, draft)
		}
		return sendMail(reply, to, subject, body, draft)
	}
	if acct != "" {
		a, ok := findAccount(acct)
		if !ok {
			return fmt.Errorf("no account called %s", acct)
		}
		return sendIMAP(a, a.yours()[0], to, subject, body, "", nil, draft)
	}
	return sendMail("", to, subject, body, draft)
}

// sendIMAP sends a plain-text message through the account's SMTP server
// and saves a copy in its Sent folder (iCloud doesn't always), or, for a
// draft, only saves it in Drafts.
func sendIMAP(a account, from, to, subject, body, inReplyTo string, refs []string, draft bool) error {
	rcpts, err := mail.ParseAddressList(to)
	if err != nil || len(rcpts) == 0 {
		return fmt.Errorf("who it goes to isn't an address: %q", to)
	}
	var h mail.Header
	h.SetDate(time.Now())
	h.SetAddressList("From", []*mail.Address{{Address: from}})
	h.SetAddressList("To", rcpts)
	h.SetSubject(subject)
	domain := "heypad.local"
	if i := strings.LastIndexByte(from, '@'); i >= 0 {
		domain = from[i+1:]
	}
	if err := h.GenerateMessageIDWithHostname(domain); err != nil {
		return err
	}
	if inReplyTo != "" {
		h.SetMsgIDList("In-Reply-To", []string{inReplyTo})
	}
	if len(refs) > 0 {
		h.SetMsgIDList("References", refs)
	}
	h.SetContentType("text/plain", map[string]string{"charset": "utf-8"})
	h.Set("Content-Transfer-Encoding", "quoted-printable")
	var buf bytes.Buffer
	w, err := mail.CreateSingleInlineWriter(&buf, h)
	if err != nil {
		return err
	}
	w.Write([]byte(strings.ReplaceAll(strings.TrimRight(body, "\n")+"\n", "\n", "\r\n")))
	if err := w.Close(); err != nil {
		return err
	}
	msg := buf.Bytes()

	if draft {
		return appendMessage(a, firstNonEmpty(a.Drafts, "Drafts"), msg, []imap.Flag{imap.FlagDraft, imap.FlagSeen})
	}
	pw, err := a.password()
	if err != nil {
		return err
	}
	var addrs []string
	for _, r := range rcpts {
		addrs = append(addrs, r.Address)
	}
	if err := smtpSend(a, pw, from, addrs, msg); err != nil {
		return err
	}
	if a.Sent != "" {
		if err := appendMessage(a, a.Sent, msg, []imap.Flag{imap.FlagSeen}); err != nil {
			return fmt.Errorf("sent, but not saved in %s: %w", a.Sent, err)
		}
	}
	return nil
}

// smtpSend sends over STARTTLS, which it insists on: the password never
// goes out in the clear.
func smtpSend(a account, pw, from string, to []string, msg []byte) error {
	host, _, err := net.SplitHostPort(a.SMTP)
	if err != nil {
		return err
	}
	conn, err := net.DialTimeout("tcp", a.SMTP, 20*time.Second)
	if err != nil {
		return err
	}
	conn.SetDeadline(time.Now().Add(2 * time.Minute))
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if ok, _ := c.Extension("STARTTLS"); !ok {
		return errors.New(host + " doesn't offer STARTTLS; not sending the password in the clear")
	}
	if err := c.StartTLS(&tls.Config{ServerName: host}); err != nil {
		return err
	}
	if err := c.Auth(smtp.PlainAuth("", a.User, pw, host)); err != nil {
		return err
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	for _, r := range to {
		if err := c.Rcpt(r); err != nil {
			return err
		}
	}
	wc, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := wc.Write(msg); err != nil {
		return err
	}
	if err := wc.Close(); err != nil {
		return err
	}
	return c.Quit()
}
