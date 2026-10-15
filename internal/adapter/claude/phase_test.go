package claude

import (
	"testing"

	"github.com/nesiler/otag/internal/adapter"
)

// AskUserQuestion and ExitPlanMode start no tool (they are questions), so their
// PostToolUse ends none: tool starts and ends stay balanced.
func TestQuestionToolsHaveNoToolPhase(t *testing.T) {
	for _, tool := range []string{"AskUserQuestion", "ExitPlanMode", "Bash"} {
		raw := []byte(`{"session_id":"s","hook_event_name":"PostToolUse","tool_name":"` + tool + `","tool_input":{}}`)
		res, err := New().ParseEvent(adapter.HookContext{}, "PostToolUse", raw)
		if err != nil || len(res.Events) != 1 {
			t.Fatalf("%s: %v %+v", tool, err, res.Events)
		}
		want := ""
		if tool == "Bash" {
			want = adapter.ToolEnd
		}
		if res.Events[0].ToolPhase != want {
			t.Fatalf("%s: phase %q", tool, res.Events[0].ToolPhase)
		}
	}
}
