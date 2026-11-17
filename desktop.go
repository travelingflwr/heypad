package main

// Desktop integration, so heypad feels like the system's mail app rather
// than something you remember to open in a terminal:
//
//   heypad watch     runs in the background (a systemd user service): checks
//                    the Imbox every HEYPAD_REFRESH, keeps a status file for a
//                    top-bar widget, and sends a notification for new mail.
//   heypad status    prints that status file, for a top bar to read cheaply.
//   heypad open      focuses the mail window, or starts it: a Rex session
//                    named "heypad" running the viewer, shown in its own
//                    Rex Linux window. `heypad open mailto:x@y` composes.
//   heypad desktop-install
//                    puts the launcher entry, icon, mailto handler, service,
//                    DankMaterialShell bar widget and niri rules in place.
//
// It still runs in Rex: the window is a Rex window. Linux (niri + DMS) is
// what's wired up; on a Mac, the Rex palette (⌘⇧M) does this job.

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

//go:embed desktop
var desktopFiles embed.FS

const (
	mailSession  = "heypad" // the Rex session the mail window shows
	statusFile   = "status.json"
	maxListed    = 8 // threads the status file lists for the bar's popout
	notifyBundle = 3 // more fresh threads than this become one notification
)

type deskThread struct {
	ID      int64     `json:"id"`
	From    string    `json:"from"`
	Subject string    `json:"subject"`
	At      time.Time `json:"at"`
}

type deskStatus struct {
	New     int          `json:"new"`
	Threads []deskThread `json:"threads"` // New For You, newest first
	Checked time.Time    `json:"checked"`
	Error   string       `json:"error,omitempty"`
}

func statusPath() string { return filepath.Join(stateDir(), statusFile) }

func writeStatus(st deskStatus) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := statusPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, statusPath())
}

// runWatch is the background loop. It never notifies about mail that was
// already new when it started, so a restart doesn't replay the Imbox.
func runWatch(args []string) error {
	flags := flag.NewFlagSet("watch", flag.ContinueOnError)
	quiet := flags.Bool("quiet", false, "keep the status file, but send no notifications")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir(), 0o700); err != nil {
		return err
	}
	every := refreshInterval()
	// SIGUSR1 checks now: the bar's refresh button sends it.
	poke := make(chan os.Signal, 1)
	signal.Notify(poke, syscall.SIGUSR1)
	writePID()
	defer os.Remove(watchPIDPath())

	known := map[int64]bool{}
	first, hint := true, 0
	for {
		posts, err := loadBox("imbox", hint)
		st := deskStatus{Checked: time.Now()}
		if err != nil {
			st.Error = err.Error()
			// Keep the last good list so the bar doesn't empty on a blip.
			var old deskStatus
			if data, rerr := os.ReadFile(statusPath()); rerr == nil && json.Unmarshal(data, &old) == nil {
				st.New, st.Threads = old.New, old.Threads
			}
		} else {
			var fresh []deskThread
			now := map[int64]bool{}
			for _, p := range posts {
				if !p.New {
					continue
				}
				t := deskThread{ID: p.ID, From: p.sender(), Subject: firstNonEmpty(p.Subject, "(no subject)"), At: p.ActiveAt}
				st.New++
				if len(st.Threads) < maxListed {
					st.Threads = append(st.Threads, t)
				}
				now[p.ID] = true
				if !known[p.ID] && !first {
					fresh = append(fresh, t)
				}
			}
			known, hint = now, st.New
			if !*quiet {
				notifyNew(fresh)
			}
		}
		first = false
		if err := writeStatus(st); err != nil {
			fmt.Fprintln(os.Stderr, "heypad: writing status:", err)
		}
		select {
		case <-time.After(every):
		case <-poke:
		}
	}
}

func watchPIDPath() string { return filepath.Join(stateDir(), "watch.pid") }

func writePID() { os.WriteFile(watchPIDPath(), []byte(fmt.Sprint(os.Getpid())), 0o600) }

// pokeWatch asks a running watcher to check now.
func pokeWatch() error {
	data, err := os.ReadFile(watchPIDPath())
	if err != nil {
		return errors.New("heypad watch isn't running")
	}
	var pid int
	if _, err := fmt.Sscan(string(data), &pid); err != nil || pid <= 0 {
		return errors.New("heypad watch isn't running")
	}
	return syscall.Kill(pid, syscall.SIGUSR1)
}

func notifyNew(fresh []deskThread) {
	switch {
	case len(fresh) == 0:
		return
	case len(fresh) > notifyBundle:
		var names []string
		for _, t := range fresh[:notifyBundle] {
			names = append(names, t.From)
		}
		go notify(fmt.Sprintf("%d new emails", len(fresh)), strings.Join(names, ", ")+", and more")
	default:
		for _, t := range fresh {
			go notify(t.From, t.Subject)
		}
	}
}

// notify shows a desktop notification. On Linux, clicking it opens the mail
// window; notify-send --wait holds until then, so it runs on its own.
func notify(title, body string) {
	title, body = clean(title), clean(body)
	switch runtime.GOOS {
	case "darwin":
		script := fmt.Sprintf("display notification %q with title \"HEY\" subtitle %q", body, title)
		exec.Command("osascript", "-e", script).Run()
	default:
		// Wait for a click for a while, not forever: some notification
		// servers never say a notification closed, which would leave one
		// notify-send per email running.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		out, err := exec.CommandContext(ctx, "notify-send", "--app-name=HEY", "--icon=heypad",
			"--hint=string:desktop-entry:heypad", "--action=default=Open", "--wait",
			title, body).Output()
		if err == nil && strings.TrimSpace(string(out)) == "default" {
			openWindow()
		}
	}
}

// clean drops control characters from mail text before it reaches another
// program: a subject is someone else's words.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return ' '
		}
		return r
	}, s)
}

func runStatus(args []string) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	refresh := flags.Bool("refresh", false, "ask heypad watch to check now")
	asJSON := flags.Bool("json", false, "print the status file as it is")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *refresh {
		return pokeWatch()
	}
	data, err := os.ReadFile(statusPath())
	if err != nil {
		if *asJSON {
			fmt.Println(`{"new":0,"threads":[],"error":"heypad watch hasn't run yet"}`)
			return nil
		}
		return errors.New("no status yet; is heypad watch running?")
	}
	if *asJSON {
		os.Stdout.Write(data)
		return nil
	}
	var st deskStatus
	if err := json.Unmarshal(data, &st); err != nil {
		return err
	}
	fmt.Printf("%d new (checked %s)\n", st.New, st.Checked.Format("15:04"))
	for _, t := range st.Threads {
		fmt.Printf("  %s — %s\n", t.From, t.Subject)
	}
	return nil
}

// runOpen focuses the mail window or starts it. A mailto: argument writes a
// message to that address instead.
func runOpen(args []string) error {
	if len(args) > 0 && strings.HasPrefix(strings.ToLower(args[0]), "mailto:") {
		return openMailto(args[0])
	}
	return openWindow()
}

func openWindow() error {
	if runtime.GOOS == "darwin" {
		return errors.New("on a Mac, open heypad from the Rex palette (⌘⇧M)")
	}
	if focusMailWindow() {
		return nil
	}
	if err := ensureMailSession(); err != nil {
		return err
	}
	return launchMailWindow()
}

// focusMailWindow finds a Rex Linux window showing the heypad session (its
// title starts with the session's label) and focuses it with niri.
func focusMailWindow() bool {
	out, err := exec.Command("niri", "msg", "--json", "windows").Output()
	if err != nil {
		return false
	}
	var wins []struct {
		ID    int64  `json:"id"`
		AppID string `json:"app_id"`
		Title string `json:"title"`
	}
	if json.Unmarshal(out, &wins) != nil {
		return false
	}
	for _, w := range wins {
		if w.AppID == "dev.shuv.rex-linux" && strings.HasPrefix(w.Title, mailSession+" — ") {
			return exec.Command("niri", "msg", "action", "focus-window", "--id", fmt.Sprint(w.ID)).Run() == nil
		}
	}
	return false
}

func mailSessionExists() bool {
	out, err := exec.Command("rex", "ls", "--json").Output()
	if err != nil {
		return false
	}
	var ls struct {
		Sessions []struct {
			Label string `json:"label"`
		} `json:"sessions"`
	}
	if json.Unmarshal(out, &ls) != nil {
		return false
	}
	for _, s := range ls.Sessions {
		if s.Label == mailSession {
			return true
		}
	}
	return false
}

// ensureMailSession starts the "heypad" Rex session running the viewer. When
// the viewer quits, the session ends, and the next open starts it again.
func ensureMailSession() error {
	if mailSessionExists() {
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	out, err := exec.Command("rex", "new", mailSession, "--window", "Mail", "--", self, "view").CombinedOutput()
	if err != nil {
		return fmt.Errorf("starting the heypad session: %v %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func launchMailWindow() error {
	cmd := exec.Command("rex-linux", "--session", mailSession, "--window-size", "1100x820")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting Rex Linux: %w", err)
	}
	return cmd.Process.Release()
}

// openMailto writes a message to a mailto: link's address in a new window of
// the heypad session, then shows that session.
func openMailto(link string) error {
	u, err := url.Parse(link)
	if err != nil {
		return fmt.Errorf("reading %s: %w", link, err)
	}
	to := u.Opaque
	if to == "" {
		to = u.Path
	}
	if dec, err := url.PathUnescape(to); err == nil {
		to = dec
	}
	to = strings.TrimSpace(clean(to))
	if to == "" || strings.HasPrefix(to, "-") {
		return fmt.Errorf("no address in %s", link)
	}
	subject := clean(u.Query().Get("subject"))
	if err := ensureMailSession(); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"run", "-s", mailSession, "--", self, "write", "--to", to}
	if subject != "" {
		args = append(args, "--subject", subject)
	}
	if out, err := exec.Command("rex", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("opening a message window: %v %s", err, strings.TrimSpace(string(out)))
	}
	if !focusMailWindow() {
		return launchMailWindow()
	}
	return nil
}

// runDesktopInstall puts every desktop piece in place. Each step says what it
// did; one that can't run here (no DMS, no niri) is skipped, not fatal.
func runDesktopInstall(args []string) error {
	if runtime.GOOS != "linux" {
		return errors.New("desktop-install sets up a Linux desktop (niri, DankMaterialShell); on a Mac use heypad rex-install")
	}
	flags := flag.NewFlagSet("desktop-install", flag.ContinueOnError)
	bind := flags.String("bind", "Mod+Shift+M", "niri key that opens mail")
	if err := flags.Parse(args); err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	cfg := firstNonEmpty(os.Getenv("XDG_CONFIG_HOME"), filepath.Join(home, ".config"))
	data := firstNonEmpty(os.Getenv("XDG_DATA_HOME"), filepath.Join(home, ".local", "share"))
	self, err := os.Executable()
	if err != nil {
		return err
	}
	vars := strings.NewReplacer("@HEYPAD@", self, "@BIND@", *bind, "@PATH@", servicePath(self))

	put := func(src, dst string) error {
		b, err := desktopFiles.ReadFile(src)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, []byte(vars.Replace(string(b))), 0o644)
	}
	step := func(what string, err error) {
		if err != nil {
			fmt.Printf("  ✗ %s: %v\n", what, err)
			return
		}
		fmt.Printf("  ✓ %s\n", what)
	}

	fmt.Println("heypad desktop-install:")
	step("icon", put("desktop/heypad.svg", filepath.Join(data, "icons/hicolor/scalable/apps/heypad.svg")))
	step("launcher entry \"HEY\"", put("desktop/heypad.desktop", filepath.Join(data, "applications/heypad.desktop")))
	exec.Command("update-desktop-database", filepath.Join(data, "applications")).Run()
	step("default for mailto: links", exec.Command("xdg-mime", "default", "heypad.desktop", "x-scheme-handler/mailto").Run())

	unit := filepath.Join(cfg, "systemd/user/heypad-watch.service")
	err = put("desktop/heypad-watch.service", unit)
	if err == nil {
		err = exec.Command("systemctl", "--user", "daemon-reload").Run()
	}
	if err == nil {
		err = exec.Command("systemctl", "--user", "enable", "--now", "heypad-watch.service").Run()
	}
	if err == nil {
		// Pick up a new binary if the service was already running.
		err = exec.Command("systemctl", "--user", "restart", "heypad-watch.service").Run()
	}
	step("background checks and notifications (heypad-watch.service)", err)

	if dir := filepath.Join(cfg, "dms"); isDir(dir) {
		plug := filepath.Join(dir, "plugins/heypad")
		err := walkPut(put, "desktop/dms", plug)
		step("top-bar widget (DankMaterialShell plugin \"heypad\")", err)
		fmt.Println("    add it to the bar: DMS Settings → Plugins → enable HEY, then Dank Bar → right section → add HEY")
	}

	if dir := filepath.Join(cfg, "niri"); isDir(dir) {
		err := put("desktop/niri.kdl", filepath.Join(dir, "heypad.kdl"))
		if err == nil {
			err = includeNiri(filepath.Join(dir, "config.kdl"), `include optional=true "heypad.kdl"`)
		}
		step(fmt.Sprintf("niri: %s opens mail; the mail window floats at the top right", *bind), err)
	}
	return nil
}

func walkPut(put func(src, dst string) error, src, dst string) error {
	return fs.WalkDir(desktopFiles, src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		return put(p, filepath.Join(dst, rel))
	})
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// includeNiri appends an include line to niri's config once.
func includeNiri(config, line string) error {
	b, err := os.ReadFile(config)
	if err != nil {
		return err
	}
	if strings.Contains(string(b), line) {
		return nil
	}
	f, err := os.OpenFile(config, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "\n// heypad: mail key and window rule\n%s\n", line)
	return err
}

// servicePath is PATH for the systemd service: heypad's folder, the hey
// CLI's, and the system's, since a user service doesn't read shell startup.
func servicePath(self string) string {
	dirs := []string{filepath.Dir(self)}
	if p, err := exec.LookPath(heyBin()); err == nil {
		dirs = append(dirs, filepath.Dir(p))
	}
	if p, err := exec.LookPath("rex"); err == nil {
		dirs = append(dirs, filepath.Dir(p))
	}
	dirs = append(dirs, "/usr/local/bin", "/usr/bin", "/bin")
	seen, out := map[string]bool{}, []string{}
	for _, d := range dirs {
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return strings.Join(out, ":")
}
