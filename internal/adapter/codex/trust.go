package codex

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nesiler/otag/internal/adapter"
)

type hookListing struct {
	Data []struct {
		Hooks []struct {
			Key     string `json:"key"`
			Hash    string `json:"currentHash"`
			Command string `json:"command"`
			Source  string `json:"source"`
		} `json:"hooks"`
	} `json:"data"`
}

// Match Claude's env -u launch scrub. Explicit account/profile/run settings win;
// this works even when tmux retained an older coordinator environment.
var scrubEnv = []string{"CODEX_HOME", "OPENAI_BASE_URL", "OPENAI_API_KEY", "OPENAI_ORG_ID", "OPENAI_PROJECT_ID", "CODEX_API_KEY", "CODEX_THREAD_ID", "CODEX_SANDBOX", "CODEX_SANDBOX_NETWORK_DISABLED", "CODEX_SANDBOX_POLICY", "CODEX_SANDBOX_TYPE", "CODEX_MANAGED_BY_NPM", "CODEX_MANAGED_BY_BUN"}

func scrubArgv(env map[string]string, argv []string) []string {
	out := []string{"/usr/bin/env"}
	for _, k := range scrubEnv {
		if _, set := env[k]; !set {
			out = append(out, "-u", k)
		}
	}
	// Unknown CODEX_SANDBOX* variables can also survive in tmux's environment.
	// Filter their names at execution time; explicit run settings still win.
	var preserved []string
	for k := range env {
		if strings.HasPrefix(k, "CODEX_SANDBOX") {
			preserved = append(preserved, shellQuote(k))
		}
	}
	sort.Strings(preserved)
	cases := ""
	if len(preserved) > 0 {
		cases = strings.Join(preserved, "|") + ") ;;"
	}
	script := "for name in $(/usr/bin/env | /usr/bin/sed -n 's/^\\(CODEX_SANDBOX[[:alnum:]_]*\\)=.*/\\1/p'); do case \"$name\" in " + cases + " *) unset \"$name\";; esac; done; exec \"$@\""
	out = append(out, "/bin/sh", "-c", script, "otag-codex-env")
	return append(out, argv...)
}

func processEnv(extra map[string]string) []string {
	env := map[string]string{}
	for _, entry := range os.Environ() {
		k, v, ok := strings.Cut(entry, "=")
		if ok {
			env[k] = v
		}
	}
	for _, k := range scrubEnv {
		delete(env, k)
	}
	for k := range env {
		if strings.HasPrefix(k, "CODEX_SANDBOX") {
			delete(env, k)
		}
	}
	for k, v := range extra {
		env[k] = v
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

// Trust only otag's exact session-flag definitions, never hooks from user or
// project config. Version, account, and declarations all participate in the key.
func hookTrust(ctx context.Context, spec adapter.RunSpec, env map[string]string, base []string, commands map[string]bool) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	versionCmd := exec.CommandContext(ctx, base[0], "--version")
	versionCmd.Env = processEnv(env)
	versionCmd.Dir = spec.Workdir
	version, err := versionCmd.Output()
	if err != nil {
		return "", fmt.Errorf("codex version: %w", err)
	}
	declarations := []string{}
	for i, arg := range base {
		if arg == "-c" && i+1 < len(base) && strings.HasPrefix(base[i+1], "hooks.") {
			declarations = append(declarations, base[i+1])
		}
	}
	keyData, _ := json.Marshal([]any{string(version), base[0], declarations, env["CODEX_HOME"]})
	hash := sha256.Sum256(keyData)
	cache := filepath.Join(spec.CacheDir, "hooks-"+hex.EncodeToString(hash[:])+".json")
	var state map[string]string
	if b, err := os.ReadFile(cache); err == nil {
		_ = json.Unmarshal(b, &state)
	}
	if len(state) != len(commands) {
		cmd := exec.CommandContext(ctx, base[0], append(base[1:], "app-server", "--listen", "stdio://")...)
		cmd.Env = processEnv(env)
		cmd.Dir = spec.Workdir
		var diagnostics bytes.Buffer
		cmd.Stderr = &diagnostics
		in, err := cmd.StdinPipe()
		if err != nil {
			return "", err
		}
		out, err := cmd.StdoutPipe()
		if err != nil {
			return "", err
		}
		if err := cmd.Start(); err != nil {
			return "", err
		}
		defer func() { _ = in.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() }()
		enc := json.NewEncoder(in)
		scanner := bufio.NewScanner(out)
		scanner.Buffer(make([]byte, 4096), 4<<20)
		request := func(id int, method string, params any, result any) error {
			if err := enc.Encode(map[string]any{"id": id, "method": method, "params": params}); err != nil {
				return err
			}
			for scanner.Scan() {
				var msg envelope
				if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
					return err
				}
				if string(msg.ID) != fmt.Sprint(id) {
					continue
				}
				if len(msg.Error) > 0 {
					return fmt.Errorf("codex %s: %s", method, msg.Error)
				}
				if result != nil {
					return json.Unmarshal(msg.Result, result)
				}
				return nil
			}
			if err := scanner.Err(); err != nil {
				return err
			}
			_ = cmd.Wait()
			return fmt.Errorf("codex %s closed before response: %s", method, diagnostics.String())
		}
		if err := request(1, "initialize", initializeParams(), nil); err != nil {
			return "", err
		}
		if err := enc.Encode(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
			return "", err
		}
		var listing hookListing
		if err := request(2, "hooks/list", map[string]any{"cwds": []string{spec.Workdir}}, &listing); err != nil {
			return "", err
		}
		state = map[string]string{}
		for _, group := range listing.Data {
			for _, hook := range group.Hooks {
				if hook.Source == "sessionFlags" && commands[hook.Command] && hook.Key != "" && hook.Hash != "" {
					state[hook.Key] = hook.Hash
				}
			}
		}
		if len(state) != len(commands) {
			return "", fmt.Errorf("codex hooks/list found %d of %d otag hooks", len(state), len(commands))
		}
		if spec.CacheDir != "" {
			if err := os.MkdirAll(spec.CacheDir, 0o700); err != nil {
				return "", err
			}
			b, _ := json.Marshal(state)
			tmp, err := os.CreateTemp(spec.CacheDir, ".hooks-")
			if err != nil {
				return "", err
			}
			name := tmp.Name()
			defer func() { _ = os.Remove(name) }()
			if _, err = tmp.Write(b); err != nil {
				_ = tmp.Close()
				return "", err
			}
			if err = tmp.Close(); err != nil {
				return "", err
			}
			if err = os.Rename(name, cache); err != nil {
				return "", err
			}
		}
	}
	keys := make([]string, 0, len(state))
	for k := range state {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	entries := make([]string, 0, len(keys))
	for _, k := range keys {
		entries = append(entries, quote(k)+"={trusted_hash="+quote(state[k])+"}")
	}
	return "{" + strings.Join(entries, ",") + "}", nil
}
