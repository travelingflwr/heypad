package main

// `heypad write` runs in the floating pane the viewer opens for a reply or a
// new message. It asks for what it needs, opens your editor, shows what you
// wrote, and sends only when you say so. It leaves its outcome in a file for
// the viewer, which is waiting for the pane to close.

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	ansiBold  = "\x1b[1m"
	ansiDim   = "\x1b[2m"
	ansiReset = "\x1b[0m"
	ansiBlue  = "\x1b[34m"
	ansiRed   = "\x1b[31m"
	ansiGreen = "\x1b[32m"
)

func runWrite(args []string) error {
	fs := flag.NewFlagSet("write", flag.ContinueOnError)
	token := fs.String("result", "", "save the outcome under this token")
	reply := fs.String("reply", "", "thread ID to reply to")
	subject := fs.String("subject", "", "the thread's subject, or a new message's")
	to := fs.String("to", "", "who it goes to")
	acct := fs.String("account", "", "send a new message from this account instead of HEY")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// These come from the message being answered, and are printed below.
	*subject, *to = termSafe(*subject), termSafe(*to)
	res := writeResult{Message: "Cancelled"}
	defer func() {
		if *token != "" {
			data, _ := json.Marshal(res)
			os.MkdirAll(stateDir(), 0o700)
			os.WriteFile(resultPath(*token), data, 0o600)
		}
	}()
	in := bufio.NewReader(os.Stdin)
	ask := func(prompt string) string {
		fmt.Printf("%s%s%s ", ansiBold, prompt, ansiReset)
		line, _ := in.ReadString('\n')
		return strings.TrimSpace(line)
	}

	fmt.Printf("\x1b]2;%s\x07\x1b[H\x1b[2J", termSafe(firstNonEmpty(*acct, "HEY")))
	if *reply != "" {
		fmt.Printf("%sReply%s %sto %s%s\n%s%s%s\n\n", ansiBold+ansiBlue, ansiReset, ansiDim, *to, ansiReset, ansiBold, *subject, ansiReset)
	} else {
		fmt.Printf("%sNew message%s\n\n", ansiBold+ansiBlue, ansiReset)
		if *to == "" {
			*to = ask("To:")
		}
		if *to == "" {
			return pause("Nothing to send.", false)
		}
		if *subject == "" {
			*subject = ask("Subject:")
		}
		if *subject == "" {
			return pause("A new message needs a subject.", false)
		}
	}

	body, err := editText("")
	if err != nil {
		res.Message = "Couldn't open an editor: " + err.Error()
		return pause(res.Message, true)
	}
	if strings.TrimSpace(body) == "" {
		return pause("Empty message, nothing sent.", false)
	}
	for {
		fmt.Print("\x1b[H\x1b[2J")
		if *reply != "" {
			fmt.Printf("%sReply to %s%s\n%s%s%s\n", ansiBold, *to, ansiReset, ansiDim, *subject, ansiReset)
		} else {
			fmt.Printf("%sTo:%s %s\n%sSubject:%s %s\n", ansiBold, ansiReset, *to, ansiBold, ansiReset, *subject)
		}
		fmt.Printf("%s%s%s\n%s\n%s%s%s\n\n", ansiDim, strings.Repeat("─", 40), ansiReset, strings.TrimRight(body, "\n"), ansiDim, strings.Repeat("─", 40), ansiReset)
		choice := strings.ToLower(ask("Send it? [s]end, [e]dit, save as [d]raft, [c]ancel:"))
		switch {
		case strings.HasPrefix(choice, "s") || choice == "y" || choice == "yes":
			if err := sendFor(*acct, *reply, *to, *subject, body, false); err != nil {
				res.Message = "Not sent: " + err.Error()
				return pause(res.Message, true)
			}
			res = writeResult{OK: true, Message: "Sent"}
			fmt.Printf("\n%sSent ✓%s\n", ansiGreen, ansiReset)
			time.Sleep(700 * time.Millisecond)
			return nil
		case strings.HasPrefix(choice, "d"):
			if err := sendFor(*acct, *reply, *to, *subject, body, true); err != nil {
				res.Message = "Draft not saved: " + err.Error()
				return pause(res.Message, true)
			}
			res = writeResult{OK: true, Message: "Saved as a draft"}
			fmt.Printf("\n%sSaved as a draft ✓%s\n", ansiGreen, ansiReset)
			time.Sleep(700 * time.Millisecond)
			return nil
		case strings.HasPrefix(choice, "e"):
			if body, err = editText(body); err != nil {
				res.Message = "Couldn't open an editor: " + err.Error()
				return pause(res.Message, true)
			}
		case strings.HasPrefix(choice, "c") || strings.HasPrefix(choice, "q") || choice == "n":
			return nil
		}
	}
}

func sendMail(reply, to, subject, body string, draft bool) error {
	var args []string
	if reply != "" {
		args = []string{"reply", reply, "-m", body}
	} else {
		args = []string{"compose", "--to", to, "--subject", subject, "-m", body}
	}
	if draft {
		args = append(args, "--draft")
	}
	_, err := heyJSON(args...)
	return err
}

// editText opens $VISUAL or $EDITOR (vi if neither) on text and returns
// what was saved.
func editText(text string) (string, error) {
	f, err := os.CreateTemp("", "heypad-*.md")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	f.WriteString(text)
	f.Close()
	editor := firstNonEmpty(os.Getenv("VISUAL"), os.Getenv("EDITOR"), "vi")
	cmd := exec.Command("/bin/sh", "-c", editor+` "$1"`, "sh", f.Name())
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return "", err
	}
	data, err := os.ReadFile(f.Name())
	return string(data), err
}

// pause shows msg, and for a failure waits for Return so it can be read
// before the pane closes.
func pause(msg string, failed bool) error {
	if !failed {
		fmt.Printf("\n%s%s%s\n", ansiDim, msg, ansiReset)
		time.Sleep(900 * time.Millisecond)
		return nil
	}
	fmt.Printf("\n%s%s%s\n%sPress Return to close.%s", ansiRed, msg, ansiReset, ansiDim, ansiReset)
	bufio.NewReader(os.Stdin).ReadString('\n')
	return nil
}
