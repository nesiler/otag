package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func (h *harness) runStdin(stdin string, args ...string) (int, string) {
	var out, errb bytes.Buffer
	code := Main(context.Background(), args, Env{
		Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errb, Cwd: h.cwd,
		Getenv: func(k string) string { return h.env[k] },
	})
	return code, out.String() + errb.String()
}

func TestStatuslineAndQuota(t *testing.T) {
	h := newHarness(t)
	reset := time.Now().Add(90 * time.Minute).Unix()
	payload := fmt.Sprintf(`{"rate_limits":{"five_hour":{"used_percentage":100,"resets_at":%d},`+
		`"seven_day":{"used_percentage":40.5,"resets_at":%d}}}`, reset, reset+86400)

	// The wrapper prints only the wrapped command's output, and always exits 0.
	code, out := h.runStdin(payload, "statusline", "--account", "c1", "--command", "printf 'my line'")
	if code != 0 || out != "my line" {
		t.Fatalf("exit %d output %q", code, out)
	}
	if code, out := h.runStdin("{broken", "statusline", "--account", "nobody", "--command", "printf ok"); code != 0 || out != "ok" {
		t.Fatalf("broken payload: exit %d %q", code, out)
	}

	var env struct {
		Data struct {
			Snapshots []struct {
				Account     string   `json:"account"`
				Window      string   `json:"window"`
				UsedPercent *float64 `json:"used_percent"`
				Source      string   `json:"source"`
			} `json:"snapshots"`
			Cooldowns []struct {
				Account string `json:"account"`
			} `json:"cooldowns"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(h.ok("--json", "quota")), &env); err != nil {
		t.Fatal(err)
	}
	s := env.Data.Snapshots
	if len(s) != 2 || s[0].Window != "5h" || *s[0].UsedPercent != 100 || s[1].Window != "7d" ||
		s[0].Source != "claude_statusline" || len(env.Data.Cooldowns) != 1 || env.Data.Cooldowns[0].Account != "c1" {
		t.Fatalf("quota %+v", env.Data)
	}
	human := h.ok("quota")
	for _, want := range []string{"ACCOUNT", "c1", "5h", "100%", "1h29m", "claude_statusline", "cooldown: c1 until"} {
		if !strings.Contains(human, want) {
			t.Fatalf("quota output lacks %q:\n%s", want, human)
		}
	}
	h.cwd = h.alpha
	status := h.ok("status")
	if !strings.Contains(status, "QUOTA") || !strings.Contains(status, "c1: 5h 100%") {
		t.Fatalf("status:\n%s", status)
	}
	if !strings.Contains(h.ok("--json", "status"), `"quota":[{"account":"c1"`) {
		t.Fatal("status --json lacks quota")
	}
	// --refresh works with nothing to collect (claude without oauth_usage).
	h.ok("quota", "--refresh", "--account", "c1")
}
