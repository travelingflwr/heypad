package main

// Debug snapshots: `heypad snapshot` asks running viewers (by SIGUSR1) to
// save what they're showing as a PNG, put together from the same images
// they sent the terminal, plus their state as JSON.

import (
	"encoding/json"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func snapshotPath(pid int, ext string) string {
	return filepath.Join(stateDir(), fmt.Sprintf("snapshot-%d.%s", pid, ext))
}

// writeSnapshot lays the header over the sheet's crop, with the selection
// under it, on the theme's background, the way the terminal stacks them.
func writeSnapshot(g geom, pal palette, hdr, sheet *image.RGBA, sy int, hl *image.RGBA, hlRow int, info map[string]any) {
	img := image.NewRGBA(image.Rect(0, 0, g.widthPx, g.hdrPx+g.viewPx))
	draw.Draw(img, img.Bounds(), image.NewUniform(pal.bg), image.Point{}, draw.Src)
	view := image.Rect(0, g.hdrPx, g.widthPx, g.hdrPx+g.viewPx)
	if hl != nil {
		at := image.Pt(0, g.hdrPx+hlRow*g.cellH)
		draw.Draw(img, hl.Bounds().Add(at).Intersect(view), hl, image.Point{}, draw.Over)
	}
	if sheet != nil {
		draw.Draw(img, view, sheet, image.Pt(0, sy), draw.Over)
	}
	if hdr != nil {
		draw.Draw(img, image.Rect(0, 0, g.widthPx, g.hdrPx), hdr, image.Point{}, draw.Over)
	}
	info["time"] = time.Now().Format(time.RFC3339)
	info["note"] = "edge fades aren't drawn in snapshots"
	os.MkdirAll(stateDir(), 0o700)
	if f, err := os.Create(snapshotPath(os.Getpid(), "png")); err == nil {
		png.Encode(f, img)
		f.Close()
	}
	data, _ := json.MarshalIndent(info, "", "  ")
	os.WriteFile(snapshotPath(os.Getpid(), "json"), data, 0o600)
}

// runSnapshot signals every running viewer and prints where they saved.
func runSnapshot() error {
	out, err := exec.Command("pgrep", "-f", "heypad view").Output()
	if err != nil {
		return fmt.Errorf("no heypad viewer is running")
	}
	for _, f := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(f)
		if err != nil || pid == os.Getpid() {
			continue
		}
		os.Remove(snapshotPath(pid, "png"))
		if syscall.Kill(pid, syscall.SIGUSR1) != nil {
			continue
		}
		for i := 0; i < 40; i++ {
			if _, err := os.Stat(snapshotPath(pid, "json")); err == nil {
				if _, err := os.Stat(snapshotPath(pid, "png")); err == nil {
					break
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
		fmt.Println(snapshotPath(pid, "png"))
		fmt.Println(snapshotPath(pid, "json"))
	}
	return nil
}
