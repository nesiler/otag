package quota

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Claude OAuth usage: an undocumented endpoint, read with the access token Claude Code
// keeps in the macOS Keychain. Off by default (accounts.<name>.quota.oauth_usage). otag
// only reads the token: an expired token is skipped, never refreshed (refreshing would
// rotate Claude Code's refresh token behind its back).
const (
	ClaudeUsageURL       = "https://api.anthropic.com/api/oauth/usage"
	claudeKeychainPrefix = "Claude Code-credentials"
)

// errTokenExpired means the Keychain token is past its expiry; Claude Code renews it on
// its next use.
var errTokenExpired = errors.New("claude oauth token expired; waiting for Claude Code to refresh it")

// KeychainServices are the Keychain service names Claude Code may use for a config
// dir, in lookup order: "Claude Code-credentials-<sha256(dir)[:8]>" whenever
// CLAUDE_CONFIG_DIR is set; the plain name when it is unset, which is how ~/.claude
// is used (checked first for it).
func KeychainServices(configDir, home string) []string {
	def := filepath.Join(home, ".claude")
	if configDir == "" {
		return []string{claudeKeychainPrefix}
	}
	sum := sha256.Sum256([]byte(filepath.Clean(configDir)))
	hashed := claudeKeychainPrefix + "-" + hex.EncodeToString(sum[:])[:8]
	if filepath.Clean(configDir) == def {
		// otag runs leave CLAUDE_CONFIG_DIR unset for ~/.claude, as a plain `claude` does.
		return []string{claudeKeychainPrefix, hashed}
	}
	return []string{hashed}
}

// KeychainReader returns the secret of a generic password item; injected in tests.
type KeychainReader func(ctx context.Context, service string) ([]byte, error)

// SecurityCLI reads a generic password with /usr/bin/security (read-only; it never
// changes the item or its ACL).
func SecurityCLI(ctx context.Context, service string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "/usr/bin/security", "find-generic-password", "-s", service, "-w")
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("keychain item %q not readable", service)
	}
	return bytes.TrimSpace(out.Bytes()), nil
}

// claudeToken finds a usable access token for the config dir.
func claudeToken(ctx context.Context, read KeychainReader, configDir, home string, now time.Time) (string, error) {
	last := errors.New("no keychain item")
	for _, svc := range KeychainServices(configDir, home) {
		b, err := read(ctx, svc)
		if err != nil {
			last = err
			continue
		}
		var c struct {
			OAuth struct {
				AccessToken string `json:"accessToken"`
				ExpiresAt   int64  `json:"expiresAt"` // ms
			} `json:"claudeAiOauth"`
		}
		if json.Unmarshal(b, &c) != nil || c.OAuth.AccessToken == "" {
			last = fmt.Errorf("keychain item %q has no oauth token", svc)
			continue
		}
		if c.OAuth.ExpiresAt > 0 && !time.UnixMilli(c.OAuth.ExpiresAt).After(now) {
			return "", errTokenExpired
		}
		return c.OAuth.AccessToken, nil
	}
	return "", last
}

// RetryAfterError is a 429/401/403 answer: do not ask again before Until.
type RetryAfterError struct {
	Status int
	Until  time.Time
}

func (e *RetryAfterError) Error() string {
	return fmt.Sprintf("claude oauth usage: HTTP %d, retry after %s", e.Status, e.Until.Format(time.RFC3339))
}

// FetchClaudeUsage calls the usage endpoint. userAgent must look like Claude Code's
// ("claude-cli/<version> (external, cli)"); the endpoint answers 429 without one.
func FetchClaudeUsage(ctx context.Context, c *http.Client, url, token, userAgent string, now time.Time) ([]Window, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusTooManyRequests, http.StatusUnauthorized, http.StatusForbidden:
		wait := 5 * time.Minute
		if resp.StatusCode != http.StatusTooManyRequests {
			wait = 30 * time.Minute
		}
		if s, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && s > 0 {
			wait = time.Duration(s) * time.Second
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, &RetryAfterError{Status: resp.StatusCode, Until: now.Add(wait)}
	default:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, fmt.Errorf("claude oauth usage: HTTP %d", resp.StatusCode)
	}
	var body map[string]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("claude oauth usage: %w", err)
	}
	var out []Window
	for _, k := range sortedKeys(body) {
		var v struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    *string  `json:"resets_at"`
		}
		if json.Unmarshal(body[k], &v) != nil || v.Utilization == nil {
			continue // null windows, extra_usage, limits[] ...
		}
		w := Window{Name: oauthWindowName(k), UsedPercent: ptr(*v.Utilization)}
		if v.ResetsAt != nil {
			if t, err := time.Parse(time.RFC3339Nano, *v.ResetsAt); err == nil {
				w.ResetsAt = t.UTC()
			}
		}
		out = append(out, w)
	}
	return out, nil
}

// oauthWindowName: five_hour → 5h, seven_day → 7d, seven_day_opus → 7d-opus.
func oauthWindowName(k string) string {
	if n, ok := claudeWindows[k]; ok {
		return n
	}
	if rest, ok := strings.CutPrefix(k, "seven_day_"); ok {
		return "7d-" + rest
	}
	return k
}

// claudeUserAgent builds Claude Code's user agent from `<bin> --version`
// ("2.1.293 (Claude Code)").
func claudeUserAgent(ctx context.Context, bin string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	v := ""
	if err == nil {
		if f := strings.Fields(string(out)); len(f) > 0 {
			v = f[0]
		}
	}
	if v == "" {
		v = "2.1.0"
	}
	return "claude-cli/" + v + " (external, cli)"
}
