package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// thisExe reproduces the binary path installHook embeds in the Stop-hook
// command: os.Executable() with symlinks resolved. Under `go test` this is the
// compiled test binary, which is exactly what installHook writes.
func thisExe(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	return exe
}

// readSettings parses ~/.claude/settings.json under home and fails on any error,
// which also asserts the file is present and valid JSON.
func readSettings(t *testing.T, home string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("settings.json is not valid JSON: %v", err)
	}
	return m
}

// stopGroups returns the Stop-hook groups from a parsed settings map, failing if
// the shape is not the expected hooks.Stop = [] structure.
func stopGroups(t *testing.T, settings map[string]any) []any {
	t.Helper()
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("settings.hooks missing or wrong type: %T", settings["hooks"])
	}
	stop, ok := hooks["Stop"].([]any)
	if !ok {
		t.Fatalf("settings.hooks.Stop missing or wrong type: %T", hooks["Stop"])
	}
	return stop
}

// countHookGroupsWithCommand counts Stop-hook groups that contain at least one
// command hook whose command string contains substr.
func countHookGroupsWithCommand(t *testing.T, groups []any, substr string) int {
	t.Helper()
	n := 0
	for _, g := range groups {
		if groupContains(g, substr) {
			n++
		}
	}
	return n
}

// assertStopHookCommand asserts the (single) Stop hook group references this
// binary and the `hook` subcommand, with the expected command-hook fields.
func assertStopHookCommand(t *testing.T, groups []any, exe string) {
	t.Helper()
	var cmds []string
	for _, g := range groups {
		gm, ok := g.(map[string]any)
		if !ok {
			continue
		}
		hs, ok := gm["hooks"].([]any)
		if !ok {
			continue
		}
		for _, h := range hs {
			hm, ok := h.(map[string]any)
			if !ok {
				continue
			}
			if hm["type"] != "command" {
				t.Errorf("hook type = %v, want \"command\"", hm["type"])
			}
			if c, ok := hm["command"].(string); ok {
				cmds = append(cmds, c)
			}
		}
	}
	var match string
	for _, c := range cmds {
		if strings.Contains(c, exe) {
			match = c
			break
		}
	}
	if match == "" {
		t.Fatalf("no Stop hook command references this binary %q; got %v", exe, cmds)
	}
	if !strings.HasSuffix(match, " hook") {
		t.Errorf("hook command %q does not invoke the `hook` subcommand", match)
	}
}

func TestInstallHook(t *testing.T) {
	exe := thisExe(t)

	t.Run("creates settings.json when absent", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		if !installHook() {
			t.Fatal("installHook() = false, want true")
		}

		settings := readSettings(t, home)
		groups := stopGroups(t, settings)
		if got := countHookGroupsWithCommand(t, groups, exe); got != 1 {
			t.Fatalf("Stop hook groups referencing this binary = %d, want 1", got)
		}
		assertStopHookCommand(t, groups, exe)
	})

	t.Run("preserves unrelated keys and prunes stale entry", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		claudeDir := filepath.Join(home, ".claude")
		if err := os.MkdirAll(claudeDir, 0o755); err != nil {
			t.Fatal(err)
		}
		// Existing settings: an unrelated top-level key plus a STALE pre-rename
		// hook entry (Node's claude-says-hook.js) that must be pruned.
		pre := map[string]any{
			"model":       "claude-opus",
			"permissions": map[string]any{"allow": []any{"Bash"}},
			"hooks": map[string]any{
				"Stop": []any{
					map[string]any{
						"matcher": "*",
						"hooks": []any{
							map[string]any{
								"type":    "command",
								"command": "node /old/path/claude-says-hook.js",
								"timeout": 5,
							},
						},
					},
				},
			},
		}
		data, err := json.MarshalIndent(pre, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), data, 0o644); err != nil {
			t.Fatal(err)
		}

		if !installHook() {
			t.Fatal("installHook() = false, want true")
		}

		settings := readSettings(t, home)

		// Unrelated top-level keys preserved.
		if settings["model"] != "claude-opus" {
			t.Errorf("unrelated key model = %v, want claude-opus", settings["model"])
		}
		if _, ok := settings["permissions"].(map[string]any); !ok {
			t.Errorf("unrelated key permissions missing after install: %v", settings["permissions"])
		}

		groups := stopGroups(t, settings)

		// Stale entry pruned.
		if got := countHookGroupsWithCommand(t, groups, "claude-says-hook.js"); got != 0 {
			t.Errorf("stale hook groups remaining = %d, want 0", got)
		}
		// New entry present exactly once, no duplicates.
		if got := countHookGroupsWithCommand(t, groups, exe); got != 1 {
			t.Fatalf("Stop hook groups referencing this binary = %d, want 1", got)
		}
		assertStopHookCommand(t, groups, exe)
	})

	t.Run("write is atomic with no leftover tmp", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		if !installHook() {
			t.Fatal("installHook() = false, want true")
		}

		tmp := filepath.Join(home, ".claude", "settings.json.tmp")
		if _, err := os.Stat(tmp); !os.IsNotExist(err) {
			t.Errorf("leftover temp file %s exists (stat err=%v); write not atomic", tmp, err)
		}
		// Sanity: the real file is present and valid.
		readSettings(t, home)
	})

	t.Run("idempotent second install adds no duplicate", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		if !installHook() {
			t.Fatal("first installHook() = false, want true")
		}
		if !installHook() {
			t.Fatal("second installHook() = false, want true")
		}

		settings := readSettings(t, home)
		groups := stopGroups(t, settings)
		if got := countHookGroupsWithCommand(t, groups, exe); got != 1 {
			t.Fatalf("Stop hook groups referencing this binary after 2 installs = %d, want 1", got)
		}
	})
}
