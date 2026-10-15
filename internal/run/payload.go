package run

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
)

// Hook payloads carry file contents (Read output, Write input), prompts and command
// output, so otag does not keep them by default: raw_hook_payloads stores a compact
// extract of identifiers and kinds. With [daemon] raw_payloads = true it stores the
// payload, with likely secrets redacted and long strings clipped, for debugging; the
// daemon deletes rows older than raw_payload_retention.

// compactKeys are the top-level payload fields kept: identifiers and kinds, never
// content (tool input and output, prompts, messages).
var compactKeys = []string{"hook_event_name", "session_id", "sessionID", "thread_id", "threadId", "turn_id", "turnId",
	"tool_use_id", "tool_name", "tool", "source", "reason", "type", "kind", "event_id", "id", "callID", "action",
	"effect", "status", "model", "agent_id", "agent_type", "permission_mode", "phase", "error", "code"}

// compactPayload is the allowlisted extract of a payload: the compactKeys with scalar
// values (strings clipped), and how many other fields were left out.
func compactPayload(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Sprintf(`{"otag_unparsed_bytes":%d}`, len(raw))
	}
	out := map[string]any{}
	for k, v := range m {
		if !slices.Contains(compactKeys, k) {
			continue
		}
		switch x := v.(type) {
		case string:
			out[k] = clip(redactText(x), 200)
		case float64, bool, nil:
			out[k] = x
		}
	}
	if n := len(m) - len(out); n > 0 {
		out["otag_omitted_fields"] = n
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// secretKey matches field names whose string values are credentials.
var secretKey = regexp.MustCompile(`(?i)(token|secret|passw|api[_-]?key|authorization|cookie|credential|private[_-]?key)`)

// secretText matches credentials inside text: provider keys, GitHub and Slack tokens,
// AWS access keys, JWTs, PEM private keys and NAME=value lines whose name says secret.
var secretText = regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|` +
	`AKIA[0-9A-Z]{16}|xox[abprs]-[A-Za-z0-9-]{10,}|eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}|` +
	`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----|` +
	`(?m)\b[A-Z0-9_]*(?:KEY|TOKEN|SECRET|PASSWORD|PASSWD)[A-Z0-9_]*\s*[=:]\s*\S+`)

func redactText(s string) string { return secretText.ReplaceAllString(s, "[redacted]") }

// maxField bounds one string in a debug payload.
const maxField = 4096

// redactPayload is the debug form of a payload: the JSON with secret-named fields and
// secret-looking text redacted and long strings clipped.
func redactPayload(raw []byte) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return clip(redactText(string(raw)), 64*maxField)
	}
	b, _ := json.Marshal(redactValue(v, ""))
	return string(b)
}

func redactValue(v any, key string) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = redactValue(e, k)
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = redactValue(e, key)
		}
		return x
	case string:
		if secretKey.MatchString(key) && x != "" {
			return "[redacted]"
		}
		return clip(redactText(x), maxField)
	}
	return v
}

// clip shortens s to n bytes (at a rune boundary), noting what was cut.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && s[cut]&0xc0 == 0x80 {
		cut--
	}
	return s[:cut] + fmt.Sprintf("...[%d bytes clipped]", len(s)-cut)
}
