package main

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/gobolditalic"
	"golang.org/x/image/font/gofont/goitalic"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
)

// x/image can't instance the variable SF fonts macOS ships, so prefer
// Apple's static SF Pro OTFs, then Inter, then Helvetica Neue (a collection
// every Mac has), then Go's bundled fonts so the viewer always draws.
type fontCandidate struct {
	path      string
	subfamily string // for a collection: the face to pick
}

var (
	regularCandidates = []fontCandidate{
		{"/Library/Fonts/SF-Pro-Text-Regular.otf", ""},
		{os.ExpandEnv("$HOME/Library/Fonts/SF-Pro-Text-Regular.otf"), ""},
		{"/usr/share/fonts/TTF/Inter-Regular.ttf", ""},
		{"/usr/share/fonts/truetype/inter/Inter-Regular.ttf", ""},
		{"/System/Library/Fonts/HelveticaNeue.ttc", "Regular"},
	}
	boldCandidates = []fontCandidate{
		{"/Library/Fonts/SF-Pro-Text-Semibold.otf", ""},
		{os.ExpandEnv("$HOME/Library/Fonts/SF-Pro-Text-Semibold.otf"), ""},
		{"/usr/share/fonts/TTF/Inter-SemiBold.ttf", ""},
		{"/usr/share/fonts/truetype/inter/Inter-SemiBold.ttf", ""},
		{"/System/Library/Fonts/HelveticaNeue.ttc", "Medium"},
	}
	italicCandidates = []fontCandidate{
		{"/Library/Fonts/SF-Pro-Text-RegularItalic.otf", ""},
		{os.ExpandEnv("$HOME/Library/Fonts/SF-Pro-Text-RegularItalic.otf"), ""},
		{"/usr/share/fonts/TTF/Inter-Italic.ttf", ""},
		{"/usr/share/fonts/truetype/inter/Inter-Italic.ttf", ""},
		{"/System/Library/Fonts/HelveticaNeue.ttc", "Italic"},
	}
	boldItalicCandidates = []fontCandidate{
		{"/Library/Fonts/SF-Pro-Text-SemiboldItalic.otf", ""},
		{os.ExpandEnv("$HOME/Library/Fonts/SF-Pro-Text-SemiboldItalic.otf"), ""},
		{"/usr/share/fonts/TTF/Inter-SemiBoldItalic.ttf", ""},
		{"/usr/share/fonts/truetype/inter/Inter-SemiBoldItalic.ttf", ""},
		{"/System/Library/Fonts/HelveticaNeue.ttc", "Medium Italic"},
	}
	monoCandidates = []fontCandidate{
		{"/Library/Fonts/SF-Mono-Regular.otf", ""},
		{os.ExpandEnv("$HOME/Library/Fonts/SF-Mono-Regular.otf"), ""},
		{"/System/Applications/Utilities/Terminal.app/Contents/Resources/Fonts/SF-Mono-Regular.otf", ""},
		{"/usr/share/fonts/TTF/JetBrainsMono-Regular.ttf", ""},
		{"/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf", ""},
		{"/System/Library/Fonts/Menlo.ttc", "Regular"},
	}
)

// fstyle is a font: the two the UI uses (regular and bold), and the ones
// mail bodies add (italic, bold italic, monospace for code).
type fstyle uint8

const (
	fsRegular fstyle = iota
	fsBold
	fsItalic
	fsBoldItalic
	fsMono
	nStyles
)

func styleOf(bold bool) fstyle {
	if bold {
		return fsBold
	}
	return fsRegular
}

type fontSet struct {
	fonts  [nStyles]*opentype.Font
	mu     sync.Mutex
	faces  map[faceKey]font.Face
	glyphs map[glyphKey]bool
	buf    sfnt.Buffer
}

type faceKey struct {
	style fstyle
	size  float64
}

type glyphKey struct {
	style fstyle
	r     rune
}

// loadFont returns the first candidate that parses, or the fallback; fell
// says it was the fallback.
func loadFont(env string, candidates []fontCandidate, fallback []byte) (f *opentype.Font, fell bool, err error) {
	if p := os.Getenv(env); p != "" {
		candidates = append([]fontCandidate{{p, ""}}, candidates...)
	}
	for _, c := range candidates {
		data, err := os.ReadFile(c.path)
		if err != nil {
			continue
		}
		if c.subfamily == "" {
			if f, err := opentype.Parse(data); err == nil {
				return f, false, nil
			}
			continue
		}
		coll, err := opentype.ParseCollection(data)
		if err != nil {
			continue
		}
		var buf sfnt.Buffer
		for i := 0; i < coll.NumFonts(); i++ {
			f, err := coll.Font(i)
			if err != nil {
				continue
			}
			if name, err := f.Name(&buf, sfnt.NameIDSubfamily); err == nil && strings.EqualFold(name, c.subfamily) {
				return f, false, nil
			}
		}
	}
	f, err = opentype.Parse(fallback)
	return f, true, err
}

func loadFonts() (*fontSet, error) {
	fs := &fontSet{faces: map[faceKey]font.Face{}, glyphs: map[glyphKey]bool{}}
	// Italics come from the same family as their upright weight: when the
	// regular (or bold) font fell back to Go's, so does its italic.
	var fell [nStyles]bool
	for _, l := range []struct {
		style      fstyle
		env, name  string
		candidates []fontCandidate
		fallback   []byte
	}{
		{fsRegular, "HEYPAD_FONT", "regular", regularCandidates, goregular.TTF},
		{fsBold, "HEYPAD_FONT_BOLD", "bold", boldCandidates, gobold.TTF},
		{fsItalic, "HEYPAD_FONT_ITALIC", "italic", italicCandidates, goitalic.TTF},
		{fsBoldItalic, "HEYPAD_FONT_BOLD_ITALIC", "bold italic", boldItalicCandidates, gobolditalic.TTF},
		{fsMono, "HEYPAD_FONT_MONO", "monospace", monoCandidates, gomono.TTF},
	} {
		candidates := l.candidates
		if l.style == fsItalic && fell[fsRegular] || l.style == fsBoldItalic && fell[fsBold] {
			candidates = nil
		}
		f, fb, err := loadFont(l.env, candidates, l.fallback)
		fell[l.style] = fb
		if err != nil {
			return nil, fmt.Errorf("load %s font: %w", l.name, err)
		}
		fs.fonts[l.style] = f
	}
	return fs, nil
}

// face returns a face at size device pixels.
func (fs *fontSet) face(bold bool, size float64) font.Face { return fs.styled(styleOf(bold), size) }

func (fs *fontSet) styled(st fstyle, size float64) font.Face {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	k := faceKey{st, size}
	if f, ok := fs.faces[k]; ok {
		return f
	}
	f, err := opentype.NewFace(fs.fonts[st], &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		panic(err)
	}
	fs.faces[k] = f
	return f
}

// has reports whether the font can draw r. Mail is full of emoji the text
// fonts lack; those are left out rather than drawn as boxes.
func (fs *fontSet) has(bold bool, r rune) bool { return fs.hasStyled(styleOf(bold), r) }

func (fs *fontSet) hasStyled(st fstyle, r rune) bool {
	if r < 0x80 {
		return r >= 0x20
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	k := glyphKey{st, r}
	if v, ok := fs.glyphs[k]; ok {
		return v
	}
	i, err := fs.fonts[st].GlyphIndex(&fs.buf, r)
	v := err == nil && i != 0
	fs.glyphs[k] = v
	return v
}
