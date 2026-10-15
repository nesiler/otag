package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/nesiler/otag/internal/adapter"
)

// A stable hook command keeps trust cache keys independent of RunDir. Preserve
// the first prompt per session before reporting it; a fast second turn cannot
// overwrite the rollout resume anchor before the watcher polls.
func trackSession(ctx context.Context, sc adapter.Sidecar, kind string) error {
	if kind != "SessionStart" && kind != "UserPromptSubmit" {
		return fmt.Errorf("invalid session hook %q", kind)
	}
	b, err := io.ReadAll(io.LimitReader(os.Stdin, 8<<20))
	if err != nil {
		return err
	}
	var p payload
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	if p.SessionID == "" || filepath.Base(p.SessionID) != p.SessionID {
		return fmt.Errorf("invalid session id")
	}
	dir := sc.Getenv("OTAG_CODEX_RUN_DIR")
	if dir == "" {
		return fmt.Errorf("missing Codex run directory")
	}
	if kind == "UserPromptSubmit" {
		f, err := os.OpenFile(filepath.Join(dir, kind+"-"+p.SessionID+".json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil && !os.IsExist(err) {
			return err
		}
		if err == nil {
			_, err = f.Write(b)
			closeErr := f.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		}
	} else {
		if old, err := os.ReadFile(filepath.Join(dir, "SessionStart.json")); err == nil {
			var prior payload
			if json.Unmarshal(old, &prior) == nil && prior.SessionID == p.SessionID && prior.Transcript != p.Transcript {
				_ = os.Remove(filepath.Join(dir, "UserPromptSubmit-"+p.SessionID+".json"))
			}
		}
		f, err := os.CreateTemp(dir, ".session-")
		if err != nil {
			return err
		}
		defer func() { _ = os.Remove(f.Name()) }()
		if _, err = f.Write(b); err != nil {
			_ = f.Close()
			return err
		}
		if err = f.Close(); err != nil {
			return err
		}
		if err = os.Rename(f.Name(), filepath.Join(dir, "SessionStart.json")); err != nil {
			return err
		}
	}
	_, err = sc.Emit(ctx, kind, b, "")
	return err
}

func rolloutModel(raw []byte) string {
	var frame struct {
		Type    string `json:"type"`
		Payload struct {
			Model string `json:"model"`
		} `json:"payload"`
	}
	if json.Unmarshal(raw, &frame) == nil && frame.Type == "turn_context" {
		return frame.Payload.Model
	}
	return ""
}
