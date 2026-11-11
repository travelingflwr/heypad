package main

// Images in mail. They load in the background, are kept on disk, and are
// scaled once for the size the reader draws them at.
//
// Privacy: an image fetched from the sender's own server tells them you
// opened the mail, and from where. HEY rewrites every image in a message to
// its own proxy (gopher.hey.com), which doesn't, so those load. Anything
// else stays unloaded; accounts without a proxy will need a per-message
// "Load images" (not built yet). Images inside the message (data: URIs)
// always load.

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const (
	imgMaxBytes = 12 << 20
	imgMaxSide  = 8000
	imgMaxArea  = 40_000_000
)

type imgState int

const (
	imgLoading imgState = iota
	imgReady
	imgFailed
	imgBlocked // not fetched, to keep the sender from knowing it was opened
)

type imgEntry struct {
	mu     sync.Mutex
	state  imgState
	img    image.Image
	w, h   int
	scaled *image.RGBA // the last size it was drawn at
}

func (e *imgEntry) status() (imgState, int, int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state, e.w, e.h
}

// scaledTo is the image at w×h pixels.
func (e *imgEntry) scaledTo(w, h int) *image.RGBA {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.img == nil || w <= 0 || h <= 0 {
		return nil
	}
	if e.scaled != nil && e.scaled.Bounds().Dx() == w && e.scaled.Bounds().Dy() == h {
		return e.scaled
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), e.img, e.img.Bounds(), xdraw.Src, nil)
	e.scaled = dst
	return dst
}

type imageCache struct {
	mu     sync.Mutex
	m      map[string]*imgEntry
	dir    string
	done   func(src string) // called when an image finishes loading
	client *http.Client
	slots  chan struct{} // at most this many downloads at once
	wg     sync.WaitGroup
}

func newImageCache(done func(src string)) *imageCache {
	return &imageCache{
		m:    map[string]*imgEntry{},
		dir:  filepath.Join(stateDir(), "images"),
		done: done,
		client: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// A redirect mustn't lead to a server that isn't allowed.
			if len(via) >= 5 || !remoteAllowed(req.URL) {
				return errors.New("image: redirected somewhere heypad doesn't load from")
			}
			return nil
		}},
		slots: make(chan struct{}, 4),
	}
}

// remoteAllowed says whether fetching u can't tell the sender anything.
func remoteAllowed(u *url.URL) bool {
	h := strings.ToLower(u.Hostname())
	return u.Scheme == "https" && (h == "hey.com" || strings.HasSuffix(h, ".hey.com"))
}

// get returns src's entry, starting its load the first time it's asked for.
func (c *imageCache) get(src string) *imgEntry {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[src]; ok {
		return e
	}
	e := &imgEntry{}
	c.m[src] = e
	if !strings.HasPrefix(src, "data:") {
		u, err := url.Parse(src)
		if err != nil || !remoteAllowed(u) {
			e.state = imgBlocked
			return e
		}
	}
	c.wg.Add(1)
	go c.load(src, e)
	return e
}

// wait blocks until every load started so far is done, or timeout passes.
func (c *imageCache) wait(timeout time.Duration) {
	ch := make(chan struct{})
	go func() { c.wg.Wait(); close(ch) }()
	select {
	case <-ch:
	case <-time.After(timeout):
	}
}

func (c *imageCache) load(src string, e *imgEntry) {
	defer c.wg.Done()
	data, err := c.bytes(src)
	var img image.Image
	if err == nil {
		img, err = decodeImage(data)
	}
	e.mu.Lock()
	if err != nil {
		e.state = imgFailed
	} else {
		e.state, e.img, e.w, e.h = imgReady, img, img.Bounds().Dx(), img.Bounds().Dy()
	}
	e.mu.Unlock()
	if c.done != nil {
		c.done(src)
	}
}

func (c *imageCache) bytes(src string) ([]byte, error) {
	if strings.HasPrefix(src, "data:") {
		comma := strings.IndexByte(src, ',')
		if comma < 0 || !strings.Contains(src[:comma], ";base64") {
			return nil, errors.New("unsupported data URI")
		}
		return base64.StdEncoding.DecodeString(src[comma+1:])
	}
	sum := sha256.Sum256([]byte(src))
	file := filepath.Join(c.dir, hex.EncodeToString(sum[:]))
	if data, err := os.ReadFile(file); err == nil {
		return data, nil
	}
	c.slots <- struct{}{}
	defer func() { <-c.slots }()
	req, err := http.NewRequest("GET", src, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "heypad/"+version)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("image: %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, imgMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > imgMaxBytes {
		return nil, errors.New("image too large")
	}
	if os.MkdirAll(c.dir, 0o700) == nil {
		tmp := file + ".tmp"
		if os.WriteFile(tmp, data, 0o600) == nil {
			os.Rename(tmp, file)
		}
	}
	return data, nil
}

func decodeImage(data []byte) (image.Image, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if cfg.Width > imgMaxSide || cfg.Height > imgMaxSide || cfg.Width*cfg.Height > imgMaxArea {
		return nil, errors.New("image too large")
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	return img, err
}
