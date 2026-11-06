package main

// `heypad keys` prints every key the terminal delivers, raw and decoded, so
// you can tell whether a shortcut reaches the pane at all. A key Rex binds
// never arrives here: if ⌘⇧M shows up, Rex didn't match its binding.

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

func runKeyTest() error {
	in := int(os.Stdin.Fd())
	if !term.IsTerminal(in) {
		return fmt.Errorf("heypad keys needs a terminal")
	}
	old, err := term.MakeRaw(in)
	if err != nil {
		return err
	}
	defer term.Restore(in, old)
	// The same keyboard reporting the viewer turns on.
	fmt.Print("\x1b[>1u")
	defer fmt.Print("\x1b[<u")
	fmt.Print("Press keys to see what reaches this pane. Ctrl-C quits.\r\n\r\n")
	var d keyDecoder
	buf := make([]byte, 256)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return nil
		}
		raw := strings.NewReplacer("\x1b", "ESC").Replace(fmt.Sprintf("%q", buf[:n]))
		var got []string
		quit := false
		emit := func(ev termEvent) {
			if ev.key.code != 0 {
				got = append(got, describeKey(ev.key))
				if ev.key.is('c', modCtrl) {
					quit = true
				}
			}
		}
		d.feed(buf[:n], emit)
		d.flush(emit)
		fmt.Printf("%-28s %s\r\n", raw, strings.Join(got, ", "))
		if quit {
			return nil
		}
	}
}

func describeKey(k keyPress) string {
	var parts []string
	for _, m := range []struct {
		bit  int
		name string
	}{{modSuper, "cmd"}, {modCtrl, "ctrl"}, {modAlt, "alt"}, {modShift, "shift"}} {
		if k.mods&m.bit != 0 {
			parts = append(parts, m.name)
		}
	}
	name := map[rune]string{keyUp: "up", keyDown: "down", keyLeft: "left", keyRight: "right", keyEnter: "enter",
		keyEscape: "esc", keyTab: "tab", keyBackspace: "backspace", keyHome: "home", keyEnd: "end"}[k.code]
	if name == "" {
		name = string(k.code)
	}
	return strings.Join(append(parts, name), "+")
}
