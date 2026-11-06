// heypad is a small HEY mail client drawn in a Rex pane with the kitty
// graphics protocol. Mail comes from the hey CLI; Rex supplies the rest of
// the app: a floating mail window, floating panes for writing and for asking
// Claude about a thread, and command palette actions.
package main

import (
	"errors"
	"flag"
	"fmt"
	"image"
	"image/png"
	"os"
	"time"
)

const usage = `heypad: HEY mail in a Rex pane (kitty graphics)

Usage: heypad COMMAND [ARGS]

  float [--theme T]          toggle HEY as a floating window over the current tab
  show [--below] [--ratio PCT] [--theme T]
                             open HEY in a pane beside yours (40% by default)
  hide                       close that pane
  view [--theme T] [--scale N]
                             run the mail viewer in this terminal
  write [--reply ID --subject S --to NAME]
                             write a message: editor, preview, then send
  render -o FILE.png [--box KIND] [--thread N] [--width PT] [--height PT] [--theme light|dark]
                             draw a screen to a PNG
  rex-install [--bind KEY]   write the Rex palette actions (~/.config/rex/heypad.lua)

Themes: terminal (follow Rex's theme, the default), light, dark.

In the viewer
  j k ↑ ↓      move            Enter, o   open a thread
  Tab, 1-6     switch boxes    Esc        back (minimize when floating)
  r            reply           c          new message
  a            ask Claude      O          open in HEY
  n p          next/previous thread       R   refresh      q   quit

Mail comes from the hey CLI (https://github.com/basecamp/hey-cli); sign in
with it first.
`

var version = "0.1.0"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "heypad:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		if inRex() {
			return toggleFloat("terminal")
		}
		return runView(viewOptions{theme: "terminal"})
	}
	cmd, args := args[0], args[1:]
	themeFlag := func(fs *flag.FlagSet) *string {
		return fs.String("theme", "terminal", "terminal (follow Rex's theme), light, or dark")
	}
	switch cmd {
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	case "version", "--version":
		fmt.Println("heypad", version)
		return nil
	case "float":
		fs := flag.NewFlagSet("float", flag.ContinueOnError)
		theme := themeFlag(fs)
		if err := fs.Parse(args); err != nil {
			return err
		}
		return toggleFloat(*theme)
	case "show":
		fs := flag.NewFlagSet("show", flag.ContinueOnError)
		below := fs.Bool("below", false, "split below instead of right")
		ratio := fs.Int("ratio", 40, "the pane's share of the split, percent")
		theme := themeFlag(fs)
		if err := fs.Parse(args); err != nil {
			return err
		}
		split := "right"
		if *below {
			split = "below"
		}
		return showPane(split, *ratio, *theme)
	case "hide":
		return hidePane()
	case "view":
		fs := flag.NewFlagSet("view", flag.ContinueOnError)
		theme := themeFlag(fs)
		scale := fs.Float64("scale", 0, "device pixels per point (0: from the cell size)")
		floating := fs.Bool("floating", false, "running in the floating window (Esc minimizes it)")
		if err := fs.Parse(args); err != nil {
			return err
		}
		return runView(viewOptions{theme: *theme, scale: *scale, floating: *floating})
	case "write":
		return runWrite(args)
	case "render":
		return runRender(args)
	case "rex-install":
		return runRexInstall(args)
	case "keys":
		return runKeyTest()
	}
	return fmt.Errorf("unknown command %q (try heypad help)", cmd)
}

// runRender draws one screen to a PNG, the way the viewer would: for
// checking the design without a terminal.
func runRender(args []string) error {
	fs := flag.NewFlagSet("render", flag.ContinueOnError)
	out := fs.String("o", "", "output PNG")
	kind := fs.String("box", "imbox", "the box to draw")
	thread := fs.Int("thread", -1, "draw this thread of the box (0 is the first) instead of the list")
	sel := fs.Int("sel", 0, "the selected row")
	width := fs.Float64("width", 420, "width in points")
	height := fs.Float64("height", 760, "height in points")
	scale := fs.Float64("scale", 2, "device pixels per point")
	theme := fs.String("theme", "light", "light or dark")
	fresh := fs.Int("fresh", -1, "show the first N threads as new (-1: what's really new)")
	demo := fs.Bool("demo", false, "draw a made-up mailbox instead of yours, for screenshots")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("render needs -o FILE.png")
	}
	fonts, err := loadFonts()
	if err != nil {
		return err
	}
	pal := lightPalette
	if *theme == "dark" {
		pal = darkPalette
	}
	posts := cachedBox(*kind)
	if *demo {
		posts = demoPosts(time.Now())
	}
	if len(posts) == 0 {
		if posts, err = fetchBox(*kind); err != nil {
			return err
		}
	}
	title := *kind
	for _, b := range boxes {
		if b.kind == *kind {
			title = b.title
		}
	}
	r := &renderer{fonts: fonts, scale: *scale, pal: pal, now: time.Now()}
	wPx, hdrPx := int(*width**scale), int(headerPt**scale)
	img := image.NewRGBA(image.Rect(0, 0, wPx, int(*height**scale)))
	r.paint(img)
	hdr := img.SubImage(image.Rect(0, 0, wPx, hdrPx)).(*image.RGBA)
	body := img.SubImage(image.Rect(0, hdrPx, wPx, img.Bounds().Dy())).(*image.RGBA)
	// The renderer draws from the image's origin; give it fresh images and
	// copy them in.
	hImg := image.NewRGBA(image.Rect(0, 0, hdr.Bounds().Dx(), hdr.Bounds().Dy()))
	bImg := image.NewRGBA(image.Rect(0, 0, body.Bounds().Dx(), body.Bounds().Dy()))
	r.paint(hImg)
	r.paint(bImg)
	if *thread >= 0 && *thread < len(posts) {
		p := posts[*thread]
		entries := cachedThread(p.TopicID)
		if *demo {
			entries = demoThread(p)
		}
		if len(entries) == 0 {
			if entries, err = fetchThread(p.TopicID); err != nil {
				return err
			}
		}
		r.drawHeader(hImg, header{left: "‹ " + title, leftRole: roleAccent, right: "r reply   a ask Claude   O open", rightRole: roleFaint})
		r.drawDoc(bImg, r.layoutThread(p, entries, *width, ""), 0)
	} else {
		badge := ""
		if *fresh > 0 {
			badge = fmt.Sprintf("%d new", *fresh)
		}
		r.drawHeader(hImg, header{left: title, leftBold: true, badge: badge, right: "Tab next box   c compose", rightRole: roleFaint})
		if *sel >= 0 && *sel < len(posts) {
			r.selection(bImg, rowTop(*sel))
		}
		seen := loadSeen()
		seen.baseline(*kind, posts)
		isNew := seen.isNew
		if *fresh >= 0 {
			isNew = func(p Posting) bool {
				for i := 0; i < *fresh && i < len(posts); i++ {
					if posts[i].ID == p.ID {
						return true
					}
				}
				return false
			}
		}
		r.drawList(bImg, posts, 0, "Nothing here.", isNew)
	}
	copyInto(img, hImg, 0)
	copyInto(img, bImg, hdrPx)
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

func copyInto(dst, src *image.RGBA, y int) {
	for row := 0; row < src.Bounds().Dy() && y+row < dst.Bounds().Dy(); row++ {
		copy(dst.Pix[dst.PixOffset(0, y+row):][:src.Bounds().Dx()*4], src.Pix[src.PixOffset(0, row):][:src.Bounds().Dx()*4])
	}
}
