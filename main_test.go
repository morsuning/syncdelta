// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveDataDirDefaultsToExeDir(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	want := filepath.Join(filepath.Dir(exe), "syncdelta-data")
	got, err := resolveDataDir("")
	if err != nil {
		t.Skipf("exe dir not writable here: %v", err)
	}
	defer os.RemoveAll(got)
	if got != want {
		t.Fatalf("default data dir = %q, want %q", got, want)
	}
	if home, err := os.UserConfigDir(); err == nil && strings.HasPrefix(got, filepath.Join(home, "SyncDelta")) {
		t.Fatalf("must not use the system config dir: %s", got)
	}
}

func TestResolveDataDirOverrideAndUnwritable(t *testing.T) {
	tmp := t.TempDir()
	got, err := resolveDataDir(filepath.Join(tmp, "custom"))
	if err != nil || got != filepath.Join(tmp, "custom") {
		t.Fatalf("override: %q %v", got, err)
	}
	// 目标位置被一个同名文件占着 -> 必须给出可读的错误,而不是悄悄换个地方
	blocker := filepath.Join(tmp, "blocked")
	os.WriteFile(blocker, []byte("x"), 0o644)
	if _, err := resolveDataDir(filepath.Join(blocker, "data")); err == nil || !strings.Contains(err.Error(), "--data-dir") {
		t.Fatalf("expected a helpful error, got %v", err)
	}
}
