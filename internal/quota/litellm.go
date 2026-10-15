package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// LiteLLMTarget is where and with which virtual key to ask LiteLLM. The key is never
// logged or stored.
type LiteLLMTarget struct {
	Provider string
	BaseURL  string // without /v1
	key      string
}

// errNoLiteLLM means the account's OpenCode config has no LiteLLM provider.
var errNoLiteLLM = errors.New("no litellm provider")

// OpenCodeConfigPath is the opencode.json(c) of an account: with a config_dir (the wrapper
// layout, XDG_CONFIG_HOME = <dir>/config) <dir>/config/opencode, else
// ~/.config/opencode.
func OpenCodeConfigPath(configDir, home string) string {
	dir := filepath.Join(home, ".config", "opencode")
	if configDir != "" {
		dir = filepath.Join(configDir, "config", "opencode")
	}
	for _, n := range []string{"opencode.json", "opencode.jsonc"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			return filepath.Join(dir, n)
		}
	}
	return filepath.Join(dir, "opencode.json")
}

// LoadLiteLLMTarget reads the LiteLLM provider of an OpenCode config: the provider
// named `provider`, else one with id "litellm", else the only one whose id or name
// mentions litellm. options.apiKey may be literal, {env:VAR} or {file:path} (relative
// to the config file, ~ expanded).
func LoadLiteLLMTarget(path, provider, home string, getenv func(string) string) (*LiteLLMTarget, error) {
	b, err := readSmall(path, 4<<20)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg struct {
		Provider map[string]struct {
			Name    string `json:"name"`
			Options struct {
				BaseURL string `json:"baseURL"`
				APIKey  string `json:"apiKey"`
			} `json:"options"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(stripJSONC(b), &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	id := provider
	if id == "" {
		if _, ok := cfg.Provider["litellm"]; ok {
			id = "litellm"
		} else {
			for _, k := range sortedKeys(cfg.Provider) {
				p := cfg.Provider[k]
				if strings.Contains(strings.ToLower(k+" "+p.Name), "litellm") {
					if id != "" {
						return nil, fmt.Errorf("%s: several litellm providers (%s, %s); set quota.litellm_provider", path, id, k)
					}
					id = k
				}
			}
		}
	}
	p, ok := cfg.Provider[id]
	if id == "" || !ok {
		return nil, errNoLiteLLM
	}
	if p.Options.BaseURL == "" {
		return nil, fmt.Errorf("%s: provider %s has no options.baseURL", path, id)
	}
	key, err := resolveSecret(p.Options.APIKey, filepath.Dir(path), home, getenv)
	if err != nil {
		return nil, fmt.Errorf("%s: provider %s apiKey: %w", path, id, err)
	}
	base := strings.TrimRight(p.Options.BaseURL, "/")
	base = strings.TrimSuffix(base, "/v1")
	return &LiteLLMTarget{Provider: id, BaseURL: base, key: key}, nil
}

var envRef = regexp.MustCompile(`\{env:([A-Za-z_][A-Za-z0-9_]*)\}`)

// EnvRefs lists the variables an OpenCode config refers to with {env:VAR} (sorted,
// unique); a missing or unreadable file has none.
func EnvRefs(path string) []string {
	b, err := readSmall(path, 4<<20)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	for _, m := range envRef.FindAllSubmatch(b, -1) {
		seen[string(m[1])] = true
	}
	return sortedKeys(seen)
}

// resolveSecret expands OpenCode's {env:VAR} and {file:path} references.
func resolveSecret(v, dir, home string, getenv func(string) string) (string, error) {
	switch {
	case strings.HasPrefix(v, "{env:") && strings.HasSuffix(v, "}"):
		s := getenv(v[5 : len(v)-1])
		if s == "" {
			return "", errors.New("environment variable not set")
		}
		return s, nil
	case strings.HasPrefix(v, "{file:") && strings.HasSuffix(v, "}"):
		p := v[6 : len(v)-1]
		switch {
		case p == "~" || strings.HasPrefix(p, "~/"):
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		case !filepath.IsAbs(p):
			p = filepath.Join(dir, p)
		}
		b, err := readSmall(p, 64<<10)
		if err != nil {
			return "", errors.New("key file not readable")
		}
		return strings.TrimSpace(string(b)), nil
	case v == "":
		return "", errors.New("empty")
	}
	return v, nil
}

// FetchLiteLLM asks GET <base>/key/info for the key's spend and budget. With a
// max_budget the window is "budget[-<duration>]" with used = spend / max_budget;
// without one only the spend is recorded. A blocked key counts as reached.
func FetchLiteLLM(ctx context.Context, c *http.Client, t *LiteLLMTarget) ([]Window, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.BaseURL+"/key/info", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+t.key)
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, fmt.Errorf("litellm /key/info: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Info struct {
			Spend          *float64 `json:"spend"`
			MaxBudget      *float64 `json:"max_budget"`
			BudgetResetAt  *string  `json:"budget_reset_at"`
			BudgetDuration *string  `json:"budget_duration"`
			Blocked        *bool    `json:"blocked"`
		} `json:"info"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("litellm /key/info: %w", err)
	}
	in := body.Info
	if in.Spend == nil {
		return nil, errors.New("litellm /key/info: no spend")
	}
	w := Window{Name: "budget", Used: ptr(round2(*in.Spend)), Raw: map[string]any{}}
	if in.BudgetDuration != nil && *in.BudgetDuration != "" {
		w.Name += "-" + *in.BudgetDuration
		w.Raw["budget_duration"] = *in.BudgetDuration
	}
	if in.MaxBudget != nil && *in.MaxBudget > 0 {
		w.Limit = ptr(*in.MaxBudget)
		w.UsedPercent = ptr(round2(*in.Spend / *in.MaxBudget * 100))
	}
	if in.BudgetResetAt != nil {
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999"} {
			if t, err := time.Parse(layout, *in.BudgetResetAt); err == nil {
				w.ResetsAt = t.UTC()
				break
			}
		}
	}
	if in.Blocked != nil && *in.Blocked {
		w.Reached = true
		w.Raw["blocked"] = true
	}
	return []Window{w}, nil
}

// stripJSONC removes // and /* */ comments and trailing commas outside strings, so an
// opencode.jsonc parses as JSON.
func stripJSONC(b []byte) []byte {
	out := make([]byte, 0, len(b))
	inStr, esc := false, false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if inStr {
			out = append(out, c)
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch {
		case c == '"':
			inStr = true
			out = append(out, c)
		case c == '/' && i+1 < len(b) && b[i+1] == '/':
			for i < len(b) && b[i] != '\n' {
				i++
			}
			out = append(out, '\n')
		case c == '/' && i+1 < len(b) && b[i+1] == '*':
			i += 2
			for i+1 < len(b) && (b[i] != '*' || b[i+1] != '/') {
				i++
			}
			i++
		case c == ']' || c == '}':
			j := len(out) - 1
			for j >= 0 && strings.ContainsRune(" \t\r\n", rune(out[j])) {
				j--
			}
			if j >= 0 && out[j] == ',' {
				out = append(out[:j], out[j+1:]...)
			}
			out = append(out, c)
		default:
			out = append(out, c)
		}
	}
	return out
}
