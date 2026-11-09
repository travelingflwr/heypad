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
  account add icloud --user LOGIN [--from ADDRESS]
                             add an iCloud Mail account (see heypad account)

Themes: terminal (follow Rex's theme, the default), light, dark.

In the viewer
  j k ↑ ↓      move            Enter, o   open a thread
  Tab, 1-6     switch boxes    Esc        back (minimize when floating)
  r            reply           c          new message
  a            ask Claude      O          open in HEY
  n p          next/previous thread       R   refresh      q   quit
  s            in the Imbox: only what's new, or both sections
               (the floating window starts with only what's new)
  x            mark a thread     X   mark it and every new one below
  e            mark seen (the marked ones, or this one), after a yes
  A            triage New For You with Claude

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
	case "account":
		return runAccount(args)
	case "write":
		return runWrite(args)
	case "render":
		return runRender(args)
	case "rex-install":
		return runRexInstall(args)
	case "keys":
		return runKeyTest()
	case "snapshot":
		return runSnapshot()
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
	fresh := fs.Int("fresh", -1, "mark the first N threads New For You (-1: as HEY has them)")
	floating := fs.Bool("floating", false, "draw only what's new, as the floating window does")
	demo := fs.Bool("demo", false, "draw a made-up mailbox instead of yours, for screenshots")
	fs.Int("marked", 0, "draw the first N rows marked, as for a bulk mark-seen")
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
		if posts, err = loadBox(*kind, 0); err != nil {
			return err
		}
	}
	title := *kind
	boxes = append(boxes, accountBoxes()...)
	for _, b := range boxes {
		if b.kind == *kind {
			title = b.title
		}
	}
	r := &renderer{fonts: fonts, scale: *scale, pal: pal, now: time.Now(), imgs: newImageCache(nil)}
	if *demo {
		r.triage = demoTriage()
	}
	marks := fs.Lookup("marked").Value.(flag.Getter).Get().(int)
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
			if entries, err = loadThread(p.TopicID); err != nil {
				return err
			}
		}
		r.drawHeader(hImg, header{left: "‹ " + title, leftRole: roleAccent, right: "r reply   a ask Claude   O open", rightRole: roleFaint})
		// Lay out once to start the images loading, then again with them.
		r.layoutThread(p, entries, *width, "")
		r.imgs.wait(20 * time.Second)
		r.drawDoc(bImg, r.layoutThread(p, entries, *width, ""), 0)
	} else {
		// --fresh N marks the first N as New For You; --floating draws only
		// those, the way the floating window does.
		if *fresh >= 0 {
			for i := range posts {
				posts[i].New = i < *fresh
			}
		}
		n := 0
		for _, p := range posts {
			if p.New {
				n++
			}
		}
		badge, hint := "", "Tab next box   c compose"
		if n > 0 {
			badge = fmt.Sprintf("%d new", n)
		}
		shown := posts
		if *floating {
			hint = "s all mail   c compose"
			shown = nil
			for _, p := range posts {
				if p.New {
					shown = append(shown, p)
				}
			}
		}
		r.drawHeader(hImg, header{left: title, leftBold: true, badge: badge, right: hint, rightRole: roleFaint})
		lay := layoutList(shown, *kind == "imbox" && !*floating)
		if *sel >= 0 && *sel < len(shown) {
			r.selection(bImg, lay.tops[*sel])
		}
		r.drawList(bImg, shown, lay, 0, "All caught up", *floating && len(shown) == 0)
		for i := 0; i < marks && i < len(shown); i++ {
			r.markBadge(bImg, padX+avatarR, lay.tops[i]+30)
		}
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
