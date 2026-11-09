package main

// Mail accounts besides HEY, read over IMAP and sent through SMTP. They're
// listed in ~/.config/heypad/accounts.json; each one's password lives in the
// macOS Keychain (service "heypad", account = the account's name), never in
// a file. On a machine without a Keychain, HEYPAD_PASSWORD_<NAME> works.

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/term"
)

type account struct {
	Name    string   `json:"name"`    // what heypad calls it, such as "iCloud"
	User    string   `json:"user"`    // the IMAP and SMTP login
	IMAP    string   `json:"imap"`    // host:port, implicit TLS
	SMTP    string   `json:"smtp"`    // host:port, STARTTLS
	From    string   `json:"from"`    // the address new mail is sent from
	Folders []folder `json:"folders"` // the boxes heypad shows, in order
	Sent    string   `json:"sent"`    // where sent mail is copied
	Drafts  string   `json:"drafts"`  // where drafts are saved
	Trash   string   `json:"trash"`   // where deleted mail goes
}

func (a account) trash() string { return firstNonEmpty(a.Trash, "Deleted Messages") }

type folder struct {
	Mailbox string `json:"mailbox"`
	Title   string `json:"title"`
}

// presets are the servers and folders of providers heypad knows.
var presets = map[string]account{
	"icloud": {
		Name: "iCloud",
		IMAP: "imap.mail.me.com:993",
		SMTP: "smtp.mail.me.com:587",
		Folders: []folder{
			{"INBOX", "Inbox"},
			{"Archive", "Archive"},
			{"Sent Messages", "Sent"},
		},
		Sent:   "Sent Messages",
		Drafts: "Drafts",
		Trash:  "Deleted Messages",
	},
}

func configDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "heypad")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "heypad")
}

func accountsPath() string { return filepath.Join(configDir(), "accounts.json") }

func loadAccounts() []account {
	data, err := os.ReadFile(accountsPath())
	if err != nil {
		return nil
	}
	var out []account
	if json.Unmarshal(data, &out) != nil {
		return nil
	}
	return out
}

func saveAccounts(accts []account) error {
	if err := os.MkdirAll(configDir(), 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(accts, "", "  ")
	tmp := accountsPath() + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, accountsPath())
}

func findAccount(name string) (account, bool) {
	for _, a := range loadAccounts() {
		if strings.EqualFold(a.Name, name) {
			return a, true
		}
	}
	return account{}, false
}

var reEnvName = regexp.MustCompile(`[^A-Z0-9]+`)

func (a account) password() (string, error) {
	if pw := os.Getenv("HEYPAD_PASSWORD_" + reEnvName.ReplaceAllString(strings.ToUpper(a.Name), "_")); pw != "" {
		return pw, nil
	}
	out, err := exec.Command("security", "find-generic-password", "-s", "heypad", "-a", a.Name, "-w").Output()
	if err != nil {
		return "", fmt.Errorf("no password for %s in the Keychain; run heypad account add", a.Name)
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// storePassword puts pw in the Keychain. It goes to `security` on stdin,
// not on its command line, so other processes can't see it.
func storePassword(name, pw string) error {
	if strings.ContainsAny(pw, "\"\\\n") || strings.ContainsAny(name, "\"\\\n") {
		return errors.New("that password has characters heypad can't store safely")
	}
	cmd := exec.Command("security", "-i")
	cmd.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -U -s heypad -a \"%s\" -l \"heypad: %s\" -w \"%s\"\n", name, name, pw))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("saving to the Keychain: %v %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func deletePassword(name string) {
	exec.Command("security", "delete-generic-password", "-s", "heypad", "-a", name).Run()
}

const accountUsage = `heypad account: mail accounts besides HEY

  heypad account add icloud --user LOGIN [--from ADDRESS]
        asks for an app-specific password, checks it, keeps it in the
        Keychain, and adds the account's Inbox, Archive and Sent to heypad
  heypad account list
  heypad account remove NAME

For iCloud, make an app-specific password at account.apple.com (Sign-In and
Security, App-Specific Passwords). The login is your iCloud Mail address.
`

func runAccount(args []string) error {
	if len(args) == 0 {
		fmt.Print(accountUsage)
		return nil
	}
	switch args[0] {
	case "add":
		return accountAdd(args[1:])
	case "list":
		for _, a := range loadAccounts() {
			var boxes []string
			for _, f := range a.Folders {
				boxes = append(boxes, f.Title)
			}
			fmt.Printf("%s  %s  from %s  (%s)\n", a.Name, a.User, a.From, strings.Join(boxes, ", "))
		}
		return nil
	case "remove":
		if len(args) < 2 {
			return errors.New("which account? heypad account remove NAME")
		}
		var keep []account
		found := false
		for _, a := range loadAccounts() {
			if strings.EqualFold(a.Name, args[1]) {
				found = true
				deletePassword(a.Name)
				continue
			}
			keep = append(keep, a)
		}
		if !found {
			return fmt.Errorf("no account called %s", args[1])
		}
		return saveAccounts(keep)
	}
	fmt.Print(accountUsage)
	return nil
}

func accountAdd(args []string) error {
	if len(args) == 0 {
		return errors.New("which provider? heypad account add icloud --user LOGIN")
	}
	preset, ok := presets[strings.ToLower(args[0])]
	if !ok {
		return fmt.Errorf("heypad knows these providers: icloud")
	}
	fs := flag.NewFlagSet("account add", flag.ContinueOnError)
	user := fs.String("user", "", "the login: your iCloud Mail address")
	from := fs.String("from", "", "the address to send new mail from (default: the login)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *user == "" {
		return errors.New("heypad account add icloud --user LOGIN")
	}
	a := preset
	a.User, a.From = *user, firstNonEmpty(*from, *user)

	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return errors.New("run this in a terminal: it asks for the password")
	}
	defer tty.Close()
	fmt.Fprintf(tty, "App-specific password for %s (it won't show): ", a.User)
	pwb, err := term.ReadPassword(int(tty.Fd()))
	fmt.Fprintln(tty)
	if err != nil {
		return err
	}
	pw := strings.TrimSpace(string(pwb))
	if pw == "" {
		return errors.New("no password given")
	}
	fmt.Fprintf(tty, "Signing in to %s…\n", strings.Split(a.IMAP, ":")[0])
	if err := checkLogin(a, pw); err != nil {
		hint := ""
		if !strings.Contains(strings.ToLower(a.User), "@icloud.com") && !strings.Contains(strings.ToLower(a.User), "@me.com") && !strings.Contains(strings.ToLower(a.User), "@mac.com") {
			hint = "\niCloud usually wants your @icloud.com address as the login, even when your Apple ID is another address."
		}
		return fmt.Errorf("couldn't sign in: %v%s", err, hint)
	}
	if err := storePassword(a.Name, pw); err != nil {
		return err
	}
	accts := loadAccounts()
	replaced := false
	for i := range accts {
		if strings.EqualFold(accts[i].Name, a.Name) {
			accts[i], replaced = a, true
		}
	}
	if !replaced {
		accts = append(accts, a)
	}
	if err := saveAccounts(accts); err != nil {
		return err
	}
	fmt.Fprintf(tty, "Added %s. Its boxes come after HEY's: Tab or the number keys reach them.\n", a.Name)
	w := bufio.NewWriter(tty)
	w.Flush()
	return nil
}
