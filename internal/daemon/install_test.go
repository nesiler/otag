package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The plist is written to a fresh 0600 temp file and renamed: a symlink in its place
// or a stale temp file is never written through.
func TestLaunchdInstallWritesOwnFile(t *testing.T) {
	ctx := context.Background()
	dir, home := t.TempDir(), t.TempDir()
	bin, _ := fakeLaunchctl(t, dir)
	l := Launchd{Home: home, Launchctl: bin, UID: 501, Wait: time.Second}
	agents := filepath.Join(home, "Library", "LaunchAgents")
	_ = os.MkdirAll(agents, 0o755)
	outside := filepath.Join(t.TempDir(), "outside")
	_ = os.WriteFile(outside, []byte("keep"), 0o644)
	_ = os.Symlink(outside, l.PlistFile())
	_ = os.Symlink(outside, l.PlistFile()+".tmp") // the old fixed temp name
	spec := PlistSpec{OtagBin: "/opt/otag/bin/otag", LogPath: filepath.Join(home, "daemon.log"),
		Env: map[string]string{"SECRET_KEY": "s3cr3t"}}
	if _, err := l.Install(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(outside); string(b) != "keep" {
		t.Fatalf("wrote through a symlink: %q", b)
	}
	st, err := os.Lstat(l.PlistFile())
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0o600 {
		t.Fatalf("plist %v %v", st.Mode(), err)
	}
	entries, _ := os.ReadDir(agents)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "."+Label) {
			t.Fatalf("temp file left: %s", e.Name())
		}
	}
}

// Installs are serialized: one waits for the other's lock.
func TestLaunchdInstallLock(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	bin, _ := fakeLaunchctl(t, dir)
	l := Launchd{Home: home, Launchctl: bin, UID: 501, Wait: time.Second}
	agents := filepath.Join(home, "Library", "LaunchAgents")
	_ = os.MkdirAll(agents, 0o755)
	unlock, err := lockDir(context.Background(), agents)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := l.Install(ctx, PlistSpec{OtagBin: "/opt/otag/bin/otag", LogPath: filepath.Join(home, "d.log")}); err == nil {
		t.Fatal("install ran while another held the lock")
	}
	unlock()
	if _, err := l.Install(context.Background(), PlistSpec{OtagBin: "/opt/otag/bin/otag", LogPath: filepath.Join(home, "d.log")}); err != nil {
		t.Fatal(err)
	}
}
