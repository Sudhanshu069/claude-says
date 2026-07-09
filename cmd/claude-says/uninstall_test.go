package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeSettings marshals a settings map to ~/.claude/settings.json under home.
func writeSettings(t *testing.T, home string, settings map[string]any) {
	t.Helper()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// stopGroup builds a single Stop-hook group wrapping one command hook.
func stopGroup(command string) map[string]any {
	return map[string]any{
		"matcher": "*",
		"hooks": []any{
			map[string]any{"type": "command", "command": command, "timeout": 5},
		},
	}
}

func TestRemoveHook(t *testing.T) {
	exe := thisExe(t)

	t.Run("install then remove round-trips, preserving other settings and hooks", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)

		// Seed an unrelated top-level key and an unrelated Stop hook, then install ours.
		writeSettings(t, home, map[string]any{
			"model": "claude-opus",
			"hooks": map[string]any{"Stop": []any{stopGroup("some-other-tool --run")}},
		})
		if !installHook() {
			t.Fatal("installHook() = false")
		}

		removed, err := removeHook()
		if err != nil {
			t.Fatalf("removeHook: %v", err)
		}
		if !removed {
			t.Fatal("removeHook = false, want true (our hook was installed)")
		}

		settings := readSettings(t, home)
		if settings["model"] != "claude-opus" {
			t.Errorf("unrelated key model = %v, want preserved", settings["model"])
		}
		groups := stopGroups(t, settings)
		if got := countHookGroupsWithCommand(t, groups, exe); got != 0 {
			t.Errorf("our hook still present: %d groups, want 0", got)
		}
		if got := countHookGroupsWithCommand(t, groups, "some-other-tool"); got != 1 {
			t.Errorf("unrelated Stop hook = %d, want 1 (must be preserved)", got)
		}
	})

	t.Run("removes a claude-says hook installed at a different path, and tidies empty hooks", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		// A claude-says hook whose path is NOT the current test binary — matched by
		// the `claude-says" hook` command signature, not the exe path.
		writeSettings(t, home, map[string]any{
			"hooks": map[string]any{"Stop": []any{stopGroup(`"/usr/local/bin/claude-says" hook`)}},
		})

		removed, err := removeHook()
		if err != nil || !removed {
			t.Fatalf("removeHook = (%v, %v), want (true, nil)", removed, err)
		}
		settings := readSettings(t, home)
		if _, ok := settings["hooks"]; ok {
			t.Errorf("empty hooks object not cleaned up: %v", settings["hooks"])
		}
	})

	t.Run("prunes a stale Node hook", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		writeSettings(t, home, map[string]any{
			"hooks": map[string]any{"Stop": []any{stopGroup("node /old/claude-says-hook.js")}},
		})
		removed, err := removeHook()
		if err != nil || !removed {
			t.Fatalf("removeHook = (%v, %v), want (true, nil)", removed, err)
		}
	})

	t.Run("no settings file is a no-op", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		removed, err := removeHook()
		if err != nil {
			t.Fatalf("removeHook on absent settings = err %v, want nil", err)
		}
		if removed {
			t.Error("removeHook = true on absent settings, want false")
		}
	})

	t.Run("no claude-says hook present leaves settings unchanged", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		writeSettings(t, home, map[string]any{
			"model": "x",
			"hooks": map[string]any{"Stop": []any{stopGroup("unrelated")}},
		})
		removed, err := removeHook()
		if err != nil {
			t.Fatalf("removeHook: %v", err)
		}
		if removed {
			t.Error("removeHook = true, want false (no claude-says hook)")
		}
		after := readSettings(t, home)
		if len(stopGroups(t, after)) != 1 || after["model"] != "x" {
			t.Errorf("settings changed unexpectedly: %v", after)
		}
	})
}

// runUninstall removes ~/.claude-says when it exists and honors --keep-config.
func TestRunUninstallRemovesConfigDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgDir := filepath.Join(home, ".claude-says")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	// --keep-config leaves it in place.
	if err := runUninstall(true); err != nil {
		t.Fatalf("runUninstall(keepConfig=true): %v", err)
	}
	if _, err := os.Stat(cfgDir); err != nil {
		t.Fatalf("--keep-config removed %s: %v", cfgDir, err)
	}

	// Default removes it.
	if err := runUninstall(false); err != nil {
		t.Fatalf("runUninstall(keepConfig=false): %v", err)
	}
	if _, err := os.Stat(cfgDir); !os.IsNotExist(err) {
		t.Fatalf("config dir still present after uninstall (stat err=%v)", err)
	}
}
