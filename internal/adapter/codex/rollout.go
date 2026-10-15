package codex

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nesiler/otag/internal/adapter"
)

// Mode A needs this second signal: Stop is absent on provider errors. The
// SessionStart wrapper publishes the exact rollout, so no account-wide scan or
// screen scraping is needed. Keep partial lines until the writer finishes them.
func watchRollout(ctx context.Context, sc adapter.Sidecar, sessionFile, model string) error {
	if err := sc.Ready(); err != nil {
		return err
	}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	var path, sid, currentTurn, pending, anchor string
	started := false
	var offset int64
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		if b, err := os.ReadFile(sessionFile); err == nil {
			var p payload
			if json.Unmarshal(b, &p) == nil && p.Transcript != "" && p.Transcript != path {
				path, sid = p.Transcript, p.SessionID
				offset = 0
				pending, anchor, currentTurn = "", "", ""
				started = false
			}
		}
		if path == "" {
			continue
		}
		// Resume rollouts contain old turns. Start observing at this launch's
		// first submitted turn, whose exact ID the prompt hook publishes.
		if anchor == "" {
			b, err := os.ReadFile(filepath.Join(filepath.Dir(sessionFile), "UserPromptSubmit-"+sid+".json"))
			if os.IsNotExist(err) {
				b, err = os.ReadFile(filepath.Join(filepath.Dir(sessionFile), "UserPromptSubmit.json"))
			}
			if err != nil {
				continue
			}
			var p payload
			if json.Unmarshal(b, &p) != nil || p.TurnID == "" {
				continue
			}
			anchor = p.TurnID
		}
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		buf := make([]byte, 1<<20)
		n, readErr := f.ReadAt(buf, offset)
		_ = f.Close()
		if readErr != nil && readErr != io.EOF {
			continue
		}
		offset += int64(n)
		pending += string(buf[:n])
		for {
			line, rest, ok := strings.Cut(pending, "\n")
			if !ok {
				break
			}
			pending = rest
			if m := rolloutModel([]byte(line)); m != "" {
				model = m
			}
			kind, p, tid := rolloutEvent([]byte(line), sid, currentTurn, model)
			if !started {
				if kind != "turn/started" || tid != anchor {
					continue
				}
				started = true
			}
			if rate := rolloutRateLimit([]byte(line)); rate != nil {
				b, _ := json.Marshal(map[string]any{"threadId": sid, "rateLimits": rate})
				_, _ = sc.Emit(ctx, "account/rateLimits/updated", b, "")
			}
			if tid != "" {
				currentTurn = tid
			}
			if kind != "" {
				b, _ := json.Marshal(p)
				_, _ = sc.Emit(ctx, kind, b, "")
			}
		}
		if len(pending) > 8<<20 {
			pending = ""
		} // bounded even for a corrupt rollout
	}
}

func rolloutRateLimit(raw []byte) map[string]any {
	var frame struct {
		Type    string `json:"type"`
		Payload struct {
			Type   string `json:"type"`
			Limits struct {
				Reached string `json:"rate_limit_reached_type"`
				Primary *struct {
					Used   float64 `json:"used_percent"`
					Resets int64   `json:"resets_at"`
				} `json:"primary"`
				Secondary *struct {
					Used   float64 `json:"used_percent"`
					Resets int64   `json:"resets_at"`
				} `json:"secondary"`
			} `json:"rate_limits"`
		} `json:"payload"`
	}
	if json.Unmarshal(raw, &frame) != nil || frame.Type != "event_msg" || frame.Payload.Type != "token_count" {
		return nil
	}
	rl := frame.Payload.Limits
	limited := rl.Reached != ""
	for _, w := range []*struct {
		Used   float64 `json:"used_percent"`
		Resets int64   `json:"resets_at"`
	}{rl.Primary, rl.Secondary} {
		if w != nil && w.Used >= 100 {
			limited = true
		}
	}
	if !limited {
		return nil
	}
	out := map[string]any{"rateLimitReachedType": rl.Reached}
	if rl.Primary != nil {
		out["primary"] = map[string]any{"usedPercent": rl.Primary.Used, "resetsAt": rl.Primary.Resets}
	}
	if rl.Secondary != nil {
		out["secondary"] = map[string]any{"usedPercent": rl.Secondary.Used, "resetsAt": rl.Secondary.Resets}
	}
	return out
}

func rolloutEvent(raw []byte, sid, currentTurn, model string) (string, any, string) {
	var frame struct {
		Type    string `json:"type"`
		Payload struct {
			Type    string  `json:"type"`
			TurnID  string  `json:"turn_id"`
			Message *string `json:"last_agent_message"`
			Info    *struct {
				Last struct {
					Input     int64 `json:"input_tokens"`
					Output    int64 `json:"output_tokens"`
					Cached    int64 `json:"cached_input_tokens"`
					Reasoning int64 `json:"reasoning_output_tokens"`
					Total     int64 `json:"total_tokens"`
				} `json:"last_token_usage"`
				Total struct {
					Input     int64 `json:"input_tokens"`
					Output    int64 `json:"output_tokens"`
					Cached    int64 `json:"cached_input_tokens"`
					Reasoning int64 `json:"reasoning_output_tokens"`
					Total     int64 `json:"total_tokens"`
				} `json:"total_token_usage"`
			} `json:"info"`
		} `json:"payload"`
	}
	if json.Unmarshal(raw, &frame) != nil || frame.Type != "event_msg" {
		return "", nil, ""
	}
	p := frame.Payload
	tid := p.TurnID
	if tid == "" {
		tid = currentTurn
	}
	base := map[string]any{"threadId": sid, "turnId": tid}
	switch p.Type {
	case "task_started", "turn_started":
		base["turn"] = map[string]any{"id": tid}
		return "turn/started", base, tid
	case "task_complete", "turn_complete":
		turn := map[string]any{"id": tid, "status": "completed"}
		if p.Message == nil {
			turn["status"] = "failed"
			turn["error"] = map[string]any{"message": "Codex rollout completed without an assistant answer"}
		} else {
			turn["items"] = []any{map[string]any{"type": "agentMessage", "text": *p.Message}}
		}
		base["turn"] = turn
		return "turn/completed", base, tid
	case "turn_aborted":
		base["turn"] = map[string]any{"id": tid, "status": "interrupted"}
		return "turn/completed", base, tid
	case "token_count":
		if p.Info == nil {
			return "", nil, ""
		}
		v, total := p.Info.Last, p.Info.Total
		base["model"] = model
		base["usageSource"] = "rollout"
		base["tokenUsage"] = map[string]any{"last": map[string]any{"inputTokens": v.Input, "outputTokens": v.Output, "cachedInputTokens": v.Cached, "reasoningOutputTokens": v.Reasoning, "totalTokens": v.Total}, "total": map[string]any{"inputTokens": total.Input, "outputTokens": total.Output, "cachedInputTokens": total.Cached, "reasoningOutputTokens": total.Reasoning, "totalTokens": total.Total}}
		return "thread/tokenUsage/updated", base, tid
	}
	return "", nil, ""
}
