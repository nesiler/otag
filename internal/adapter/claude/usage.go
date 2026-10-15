package claude

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"syscall"

	"github.com/nesiler/otag/internal/adapter"
)

// The transcript is read from the end: first transcriptTail bytes, then 4x more until
// the window reaches back past the turns that are reported, the file start or
// transcriptMax. A long turn (big tool results) still gets all its messages; scanning
// is a byte search plus decoding of user/assistant lines, ~10 ms per 10 MiB.
const (
	transcriptTail = 2 << 20
	transcriptMax  = 32 << 20
)

// usageTurns is how many prompts (turns) back from the end usage is reported. The
// transcript is written asynchronously, so the last message of a turn may land after
// its Stop; the next UserPromptSubmit or Stop picks it up from the previous turn, and an
// interrupted turn (no Stop) is covered by the next prompt. Each message is reported a
// few times at most; run_usage keeps one row per message id with the largest counts.
const usageTurns = 2

type transcriptLine struct {
	Type     string `json:"type"`
	PromptID string `json:"promptId"`
	Message  struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			InputTokens   int64 `json:"input_tokens"`
			OutputTokens  int64 `json:"output_tokens"`
			CacheRead     int64 `json:"cache_read_input_tokens"`
			CacheCreation int64 `json:"cache_creation_input_tokens"`
			OutputDetails struct {
				ThinkingTokens int64 `json:"thinking_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	} `json:"message"`
}

// usageEvents reads token usage of the last turns from a Claude Code transcript
// (JSONL; one line per content block, each repeating its message's usage). One event
// per API message, deduplicated on the message id. Read errors yield no events: usage
// is best effort and must never fail a hook.
func usageEvents(path string) []adapter.Event {
	if path == "" {
		return nil
	}
	f := openRegular(path)
	if f == nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	for n := int64(transcriptTail); ; n *= 4 {
		data, whole := readTail(f, st.Size(), n)
		evs, prompts := parseUsage(data)
		// Seeing a line of an earlier prompt than the reported ones proves the window
		// starts before the first reported message.
		if whole || prompts > usageTurns || n >= transcriptMax {
			return evs
		}
	}
}

// openRegular opens path for reading without blocking (a FIFO planted as the
// transcript would block a plain open) and only if it is a regular file.
func openRegular(path string) *os.File {
	if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
		return nil
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil
	}
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() { // swapped meanwhile
		_ = f.Close()
		return nil
	}
	return f
}

// readTail returns the complete lines among the last n bytes of f (size bytes long)
// and whether that is the whole file.
func readTail(f *os.File, size, n int64) ([]byte, bool) {
	off := max(size-n, 0)
	data, err := io.ReadAll(io.NewSectionReader(f, off, size-off))
	if err != nil {
		return nil, true
	}
	if off > 0 {
		// Drop the partial first line.
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			return nil, false
		}
		data = data[i+1:]
	}
	return data, off == 0
}

// parseUsage returns the usage events of the last usageTurns prompts in data and the
// number of distinct prompts seen.
func parseUsage(data []byte) ([]adapter.Event, int) {
	type msg struct {
		prompt int
		u      adapter.UsageData
	}
	var (
		order   []string
		msgs    = map[string]*msg{}
		prompts []string
	)
	for len(data) > 0 {
		var line []byte
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line, data = data[:i], data[i+1:]
		} else {
			line, data = data, nil
		}
		// Cheap filter before decoding; tool results can be large.
		isUser := bytes.Contains(line, []byte(`"type":"user"`))
		if !isUser && !bytes.Contains(line, []byte(`"type":"assistant"`)) {
			continue
		}
		var l transcriptLine
		if json.Unmarshal(line, &l) != nil {
			continue // a line being written
		}
		switch l.Type {
		case "user":
			if l.PromptID != "" && (len(prompts) == 0 || prompts[len(prompts)-1] != l.PromptID) {
				prompts = append(prompts, l.PromptID)
			}
		case "assistant":
			u := l.Message.Usage
			if l.Message.ID == "" || u == nil || l.Message.Model == "<synthetic>" {
				continue
			}
			m, ok := msgs[l.Message.ID]
			if !ok {
				m = &msg{}
				msgs[l.Message.ID] = m
				order = append(order, l.Message.ID)
			}
			// The last line of a message carries its final counts.
			m.prompt = len(prompts)
			m.u = adapter.UsageData{
				Model:            l.Message.Model,
				InputTokens:      u.InputTokens,
				OutputTokens:     u.OutputTokens,
				CacheReadTokens:  u.CacheRead,
				CacheWriteTokens: u.CacheCreation,
				ReasoningTokens:  u.OutputDetails.ThinkingTokens,
				Source:           "transcript",
				DedupeKey:        l.Message.ID,
			}
		}
	}
	first := len(prompts) - usageTurns + 1 // messages after the (usageTurns)th-last prompt
	var out []adapter.Event
	for _, id := range order {
		m := msgs[id]
		if m.prompt < first {
			continue
		}
		u := m.u
		out = append(out, adapter.Event{Type: adapter.Usage, Usage: &u,
			Data: map[string]any{"message_id": id}})
	}
	return out, len(prompts)
}
