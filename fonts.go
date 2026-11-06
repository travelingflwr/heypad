package main

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
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
)

type fontSet struct {
	regular, bold *opentype.Font
	mu            sync.Mutex
	faces         map[faceKey]font.Face
	glyphs        map[glyphKey]bool
	buf           sfnt.Buffer
}

type faceKey struct {
	bold bool
	size float64
}

type glyphKey struct {
	bold bool
	r    rune
}

func loadFont(env string, candidates []fontCandidate, fallback []byte) (*opentype.Font, error) {
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
				return f, nil
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
				return f, nil
			}
		}
	}
	return opentype.Parse(fallback)
}

func loadFonts() (*fontSet, error) {
	regular, err := loadFont("HEYPAD_FONT", regularCandidates, goregular.TTF)
	if err != nil {
		return nil, fmt.Errorf("load regular font: %w", err)
	}
	bold, err := loadFont("HEYPAD_FONT_BOLD", boldCandidates, gobold.TTF)
	if err != nil {
		return nil, fmt.Errorf("load bold font: %w", err)
	}
	return &fontSet{regular: regular, bold: bold, faces: map[faceKey]font.Face{}, glyphs: map[glyphKey]bool{}}, nil
}

// face returns a face at size device pixels.
func (fs *fontSet) face(bold bool, size float64) font.Face {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	k := faceKey{bold, size}
	if f, ok := fs.faces[k]; ok {
		return f
	}
	src := fs.regular
	if bold {
		src = fs.bold
	}
	f, err := opentype.NewFace(src, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		panic(err)
	}
	fs.faces[k] = f
	return f
}

// has reports whether the font can draw r. Mail is full of emoji the text
// fonts lack; those are left out rather than drawn as boxes.
func (fs *fontSet) has(bold bool, r rune) bool {
	if r < 0x80 {
		return r >= 0x20
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	k := glyphKey{bold, r}
	if v, ok := fs.glyphs[k]; ok {
		return v
	}
	src := fs.regular
	if bold {
		src = fs.bold
	}
	i, err := src.GlyphIndex(&fs.buf, r)
	v := err == nil && i != 0
	fs.glyphs[k] = v
	return v
}
