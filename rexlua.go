package main

// `heypad rex-install` writes a Lua module that adds HEY to Rex's command
// palette. The actions run in the Rex server itself, so "HEY: Toggle Mail"
// opens or hides the floating mail window from any tab without a shell.

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const rexModule = `-- Written by heypad rex-install. Running it again replaces this file.
-- init.lua loads it with: pcall(require, "heypad")
local heypad = %q
local label = %q
local bounds = { %g, %g, %g, %g }
local log = function(...) end -- heypad rex-install --debug replaces it

local function pane(ctx, title, argv, rect)
  if not ctx.session_id then error("run " .. title .. " from a Rex window", 0) end
  local r, err = rex.call("session.new_layer", { session_id = ctx.session_id, focus = true,
    bounds = rect,
    layout = { block = { flavor = "com.superlogical.terminal.shell", label = title,
      options = { command = argv, login = true } } } })
  if err ~= nil then error(err, 0) end
  return r
end

-- The floating HEY window: one per session, found by its label.
local function find(ctx)
  local view = rex.call("session.view", { session_id = ctx.session_id })
  for _, w in ipairs((view and view.windows) or {}) do
    for _, l in ipairs(w.layers or {}) do
      if l.kind == "floating" and l.blocks and #l.blocks == 1 and l.blocks[1].label == label then
        return l
      end
    end
  end
end

rex.action {
  name = "heypad_toggle",
  title = "HEY: Toggle Mail",
  category = "HEY",
  description = "Show or hide HEY in a floating window over this tab.",
  run = function(ctx, args)
    log("toggle", ctx, args)
    local l = find(ctx)
    if l == nil then
      return pane(ctx, label, { heypad, "view", "--floating" }, bounds)
    end
    local state = "minimized"
    if l.state == "minimized" then state = "normal" end
    local r, err = rex.call("session.set_layer_state", { session_id = ctx.session_id,
      layer_id = l.layer_id, state = state, focus = state == "normal" })
    if err ~= nil then error(err, 0) end
    return r
  end,
}

rex.action {
  name = "heypad_compose",
  title = "HEY: New Message",
  category = "HEY",
  description = "Write a new HEY message in a floating pane.",
  run = function(ctx, args)
    log("compose", ctx, args)
    return pane(ctx, "New message", { heypad, "write" }, { 0.12, 0.1, 0.76, 0.78 })
  end,
}

rex.action {
  name = "heypad_pane",
  title = "HEY: Open Mail Tab",
  category = "HEY",
  description = "Open HEY in a tab of its own.",
  run = function(ctx, args)
    log("tab", ctx, args)
    if not ctx.session_id then error("run HEY: Open Mail Tab from a Rex window", 0) end
    local r, err = rex.call("session.new_window", { session_id = ctx.session_id, focus = true,
      layout = { block = { flavor = "com.superlogical.terminal.shell", label = "HEY",
        options = { command = { heypad, "view" }, login = true } } } })
    if err ~= nil then error(err, 0) end
    return r
  end,
}
%s`

func runRexInstall(args []string) error {
	fs := flag.NewFlagSet("rex-install", flag.ContinueOnError)
	bind := fs.String("bind", "", "also bind HEY: Toggle Mail to this key, such as cmd+shift+m")
	debug := fs.Bool("debug", false, "log each action run, and bind every spelling of --bind, to find out why a key doesn't fire")
	dir := fs.String("dir", "", "the Rex config directory (default $XDG_CONFIG_HOME/rex or ~/.config/rex)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if p, err := filepath.EvalSymlinks(self); err == nil {
		self = p
	}
	if *dir == "" {
		base := os.Getenv("XDG_CONFIG_HOME")
		if base == "" {
			home, _ := os.UserHomeDir()
			base = filepath.Join(home, ".config")
		}
		*dir = filepath.Join(base, "rex")
	}
	extra := ""
	if *debug {
		logPath := filepath.Join(stateDir(), "rex-actions.log")
		extra += strings.ReplaceAll(debugLua, "LOGFILE", fmt.Sprintf("%q", logPath))
		fmt.Println("debug: actions log to", logPath)
	}
	if *bind != "" {
		if *debug {
			// Every way of writing the key, each tagged so the log says which matched.
			for _, k := range keySpellings(*bind) {
				extra += fmt.Sprintf("rex.bind(%q, \"heypad_toggle\", { via = %q })\n", k, k)
			}
		} else {
			extra += fmt.Sprintf("\nrex.bind(%q, \"heypad_toggle\")\n", physicalKey(*bind))
		}
	}
	b := floatBounds
	mod := fmt.Sprintf(rexModule, self, floatLabel, b[0], b[1], b[2], b[3], extra)
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(*dir, "heypad.lua")
	if err := os.WriteFile(path, []byte(mod), 0o644); err != nil {
		return err
	}
	fmt.Println("wrote", path)
	initPath := filepath.Join(*dir, "init.lua")
	data, _ := os.ReadFile(initPath)
	if !strings.Contains(string(data), `require, "heypad"`) {
		fmt.Printf("\nTo load it, add this line to %s, then run `rex config reload`:\n\n  pcall(require, \"heypad\")\n", initPath)
	} else {
		fmt.Println("init.lua already loads it; run `rex config reload` to pick up changes.")
	}
	return nil
}

// physicalKey names a letter key by its position (cmd+shift+m becomes
// cmd+shift+[KeyM]), so the binding matches whatever Shift does to the
// character, the way Rex's own bindings do.
func physicalKey(bind string) string {
	i := strings.LastIndex(bind, "+")
	k := bind[i+1:]
	if len(k) == 1 && (k[0] >= 'a' && k[0] <= 'z' || k[0] >= 'A' && k[0] <= 'Z') {
		return bind[:i+1] + "[Key" + strings.ToUpper(k) + "]"
	}
	return bind
}

// keySpellings is bind written every way Rex might match it.
func keySpellings(bind string) []string {
	i := strings.LastIndex(bind, "+")
	mods, k := bind[:i+1], bind[i+1:]
	// Not "cmd+M" for cmd+shift+m: Rex reads that as plain cmd+m.
	return []string{physicalKey(bind), mods + strings.ToLower(k)}
}

// debugLua logs each action run to a file: when, how it was triggered, and
// what Rex passed in.
const debugLua = `
local logfile = LOGFILE
log = function(what, ctx, args)
  local parts = { what }
  for k, v in pairs(ctx or {}) do parts[#parts + 1] = tostring(k) .. "=" .. tostring(v) end
  for k, v in pairs(args or {}) do parts[#parts + 1] = "arg." .. tostring(k) .. "=" .. tostring(v) end
  pcall(rex.exec, { "/bin/sh", "-c", 'printf "%s %s\\n" "$(date +%H:%M:%S)" "$1" >> "$2"', "sh", table.concat(parts, " "), logfile })
end
log("loaded", {}, {})
`
