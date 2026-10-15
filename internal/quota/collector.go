package quota

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nesiler/otag/internal/db"
	"github.com/nesiler/otag/internal/paths"
)

// Options configure the collectors. Zero values are production defaults.
type Options struct {
	Getenv   func(string) string
	HTTP     *http.Client // default: 5 s timeout
	Now      func() time.Time
	Keychain KeychainReader // default: SecurityCLI
	// ClaudeUsageURL overrides the OAuth usage endpoint (tests).
	ClaudeUsageURL string
	// UserAgent returns the user agent for the OAuth endpoint (default: from
	// `<claude> --version`).
	UserAgent func(ctx context.Context, bin string) string
	// LiteLLMEvery and OAuthEvery space out network calls per account
	// (defaults 2 min and 5 min). OAuthFreshness skips the OAuth call while the
	// statusLine observed the account more recently (default 10 min).
	LiteLLMEvery, OAuthEvery, OAuthFreshness time.Duration
	// Account limits Collect to one account ("" = all).
	Account string
}

// AccountQuota is the optional `quota` table of a registry account.
type AccountQuota struct {
	Disabled        bool   `json:"disabled"`
	OAuthUsage      bool   `json:"oauth_usage"`      // claude: poll the undocumented usage endpoint
	LiteLLMProvider string `json:"litellm_provider"` // opencode: provider id in opencode.json
}

type account struct {
	ID        int64
	Name      string
	Harness   string
	ConfigDir string
	Command   string
	Quota     AccountQuota
}

// Collectors polls every active account with the source of its harness. It keeps
// per-account state (file offsets, back-off) between calls, so one value should live
// as long as the daemon. Safe for concurrent use (calls are serialized).
type Collectors struct {
	db *db.DB
	o  Options

	mu        sync.Mutex
	rollouts  map[int64]*RolloutReader
	codexSeen map[int64]time.Time
	next      map[string]time.Time // "<source>:<account id>" → earliest next network call
	agents    map[string]string    // claude bin → user agent
	problems  map[string]string    // account → last source problem
	lastPrune time.Time
}

// New returns the collectors of all accounts in d.
func New(d *db.DB, o Options) *Collectors {
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 5 * time.Second}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Keychain == nil {
		o.Keychain = SecurityCLI
	}
	if o.ClaudeUsageURL == "" {
		o.ClaudeUsageURL = ClaudeUsageURL
	}
	if o.UserAgent == nil {
		o.UserAgent = claudeUserAgent
	}
	if o.LiteLLMEvery == 0 {
		o.LiteLLMEvery = 2 * time.Minute
	}
	if o.OAuthEvery == 0 {
		o.OAuthEvery = 5 * time.Minute
	}
	if o.OAuthFreshness == 0 {
		o.OAuthFreshness = 10 * time.Minute
	}
	return &Collectors{db: d, o: o, rollouts: map[int64]*RolloutReader{}, codexSeen: map[int64]time.Time{},
		next: map[string]time.Time{}, agents: map[string]string{}, problems: map[string]string{}}
}

var _ Collector = (*Collectors)(nil)

// Collect runs one round over all accounts. Unreachable endpoints, missing files and
// expired tokens are not errors (no snapshot, see Problems); database errors are.
func (c *Collectors) Collect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	accs, err := c.accounts(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, a := range accs {
		if a.Quota.Disabled {
			continue
		}
		var err error
		switch a.Harness {
		case "claude":
			err = c.claude(ctx, a)
		case "codex":
			err = c.codex(ctx, a)
		case "opencode":
			err = c.litellm(ctx, a)
		}
		var soft *softError
		switch {
		case errors.As(err, &soft):
			c.problems[a.Name] = soft.Error()
		case err != nil:
			errs = append(errs, fmt.Errorf("quota %s: %w", a.Name, err))
		default:
			delete(c.problems, a.Name)
		}
	}
	if now := c.o.Now(); now.Sub(c.lastPrune) > 24*time.Hour {
		c.lastPrune = now
		if err := c.db.Tx(ctx, func(tx *sql.Tx) error {
			_, err := Prune(ctx, tx, now.Add(-30*24*time.Hour))
			return err
		}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Problems returns the last soft failure per account (unreachable endpoint, expired
// token, unreadable config).
func (c *Collectors) Problems() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]string{}
	for k, v := range c.problems {
		out[k] = v
	}
	return out
}

// softError is a source failure that only means "no new data".
type softError struct{ err error }

func (e *softError) Error() string { return e.err.Error() }
func (e *softError) Unwrap() error { return e.err }

func soft(err error) error {
	if err == nil {
		return nil
	}
	return &softError{err}
}

func (c *Collectors) accounts(ctx context.Context) ([]account, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT id, name, harness, config_dir, command, config FROM accounts
		WHERE retired_at IS NULL AND (? = '' OR name = ?) ORDER BY name`, c.o.Account, c.o.Account)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []account
	for rows.Next() {
		var a account
		var cfg string
		if err := rows.Scan(&a.ID, &a.Name, &a.Harness, &a.ConfigDir, &a.Command, &cfg); err != nil {
			return nil, err
		}
		var conf struct {
			Quota AccountQuota `json:"quota"`
		}
		_ = json.Unmarshal([]byte(cfg), &conf)
		a.Quota = conf.Quota
		out = append(out, a)
	}
	return out, rows.Err()
}

func (c *Collectors) home() string { return paths.ExpandHome("~", c.o.Getenv) }

func (c *Collectors) record(ctx context.Context, o Observation) error {
	if len(o.Windows) == 0 {
		return nil
	}
	return c.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := Record(ctx, tx, o)
		return err
	})
}

// statusFile ingests the statusLine file of the account when it is newer than what the
// database has (the wrapper's own database write was throttled or failed).
func (c *Collectors) statusFile(ctx context.Context, a account, source string) error {
	f, ok := ReadStatusFile(StatusFilePath(paths.DataDir(c.o.Getenv), a.Name))
	if !ok || f.Source != source || len(f.Windows) == 0 {
		return nil
	}
	last, err := LatestObserved(ctx, c.db, a.ID, source)
	if err != nil {
		return err
	}
	if !f.ObservedAt.Truncate(time.Millisecond).After(last) {
		return nil
	}
	return c.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := RecordStatusline(ctx, tx, a.ID, source, f.ObservedAt, f.WindowsOf())
		return err
	})
}

func (c *Collectors) claude(ctx context.Context, a account) error {
	if err := c.statusFile(ctx, a, SourceClaudeStatusline); err != nil {
		return err
	}
	if !a.Quota.OAuthUsage {
		return nil
	}
	now := c.o.Now()
	key := SourceClaudeOAuth + ":" + fmt.Sprint(a.ID)
	if now.Before(c.next[key]) {
		return nil
	}
	if last, err := LatestObserved(ctx, c.db, a.ID, SourceClaudeStatusline); err != nil {
		return err
	} else if now.Sub(last) < c.o.OAuthFreshness {
		return nil // the statusLine is fresh enough
	}
	c.next[key] = now.Add(c.o.OAuthEvery)
	token, err := claudeToken(ctx, c.o.Keychain, a.ConfigDir, c.home(), now)
	if err != nil {
		return soft(err)
	}
	bin := a.Command
	if bin == "" {
		bin = "claude"
	}
	ua, ok := c.agents[bin]
	if !ok {
		ua = c.o.UserAgent(ctx, bin)
		c.agents[bin] = ua
	}
	ws, err := FetchClaudeUsage(ctx, c.o.HTTP, c.o.ClaudeUsageURL, token, ua, now)
	var ra *RetryAfterError
	if errors.As(err, &ra) {
		c.next[key] = ra.Until
	}
	if err != nil {
		return soft(err)
	}
	return c.record(ctx, Observation{AccountID: a.ID, Source: SourceClaudeOAuth, ObservedAt: now, Windows: ws})
}

func (c *Collectors) codex(ctx context.Context, a account) error {
	home := a.ConfigDir
	if home == "" {
		home = c.o.Getenv("CODEX_HOME")
	}
	if home == "" {
		home = filepath.Join(c.home(), ".codex")
	}
	r := c.rollouts[a.ID]
	if r == nil || r.Home != home {
		r = &RolloutReader{Home: home}
		c.rollouts[a.ID] = r
	}
	l, err := r.Latest()
	if err != nil {
		return soft(err)
	}
	if l == nil || !l.At.After(c.codexSeen[a.ID]) {
		return nil
	}
	if err := c.record(ctx, Observation{AccountID: a.ID, Source: SourceCodexRollout, ObservedAt: l.At, Windows: l.Windows}); err != nil {
		return err
	}
	c.codexSeen[a.ID] = l.At
	return nil
}

func (c *Collectors) litellm(ctx context.Context, a account) error {
	now := c.o.Now()
	key := SourceLiteLLM + ":" + fmt.Sprint(a.ID)
	if now.Before(c.next[key]) {
		return nil
	}
	c.next[key] = now.Add(c.o.LiteLLMEvery)
	path := OpenCodeConfigPath(a.ConfigDir, c.home())
	t, err := LoadLiteLLMTarget(path, a.Quota.LiteLLMProvider, c.home(), c.o.Getenv)
	if errors.Is(err, errNoLiteLLM) {
		return nil
	}
	if err != nil {
		return soft(err)
	}
	ws, err := FetchLiteLLM(ctx, c.o.HTTP, t)
	if err != nil {
		return soft(err) // unreachable: no snapshot, not an error state
	}
	return c.record(ctx, Observation{AccountID: a.ID, Source: SourceLiteLLM, ObservedAt: now, Windows: ws})
}
