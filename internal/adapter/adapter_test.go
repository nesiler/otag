package adapter

import "testing"

func TestCommandOr(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	for cmd, want := range map[string]string{
		"":                        "def",
		"oc":                      "oc",
		"/opt/bin/claude":         "/opt/bin/claude",
		"~/.opencode-work/bin/oc": "/home/u/.opencode-work/bin/oc",
		"~other/bin/x":            "~other/bin/x",
		"bin/~/x":                 "bin/~/x",
	} {
		if got := (Account{Command: cmd}).CommandOr("def"); got != want {
			t.Errorf("%q: got %q, want %q", cmd, got, want)
		}
	}
}
