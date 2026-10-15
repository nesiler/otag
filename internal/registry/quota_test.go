package registry

import (
	"strings"
	"testing"

	"github.com/nesiler/otag/internal/apperr"
)

func TestAccountQuotaTable(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, baseRegistry+"\n[accounts.claude-c1.quota]\noauth_usage = true\n", nil)
	cfg, err := Load(dir, getenv("/h"))
	if err != nil {
		t.Fatal(err)
	}
	if q := cfg.Accounts["claude-c1"].Quota; q == nil || !q.OAuthUsage {
		t.Fatalf("quota %+v", q)
	}
	if cfg.Accounts["codex"].Quota != nil || len(cfg.Warnings) != 0 {
		t.Fatalf("codex quota %+v warnings %v", cfg.Accounts["codex"].Quota, cfg.Warnings)
	}
	// An account without a quota table hashes as before.
	conf, _, _ := hashJSON(cfg.Accounts["codex"])
	if strings.Contains(conf, "quota") {
		t.Fatalf("absent quota serialized: %s", conf)
	}

	writeConfig(t, dir, baseRegistry+"\n[accounts.codex.quota]\noauth_usage = true\n", nil)
	if _, err := Load(dir, getenv("/h")); apperr.Code(err) != apperr.ConfigInvalid {
		t.Fatalf("oauth_usage on codex: %v", err)
	}
}
