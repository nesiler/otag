package codex

import (
	"slices"
	"testing"
)

// The scrub list must name the real OpenAI variables. Literal names here, not scrubEnv, so a
// bulk rename cannot change both sides at once.
func TestScrubEnvNamesRealOpenAIVars(t *testing.T) {
	for _, name := range []string{"OPENAI_BASE_URL", "OPENAI_API_KEY", "OPENAI_ORG_ID", "OPENAI_PROJECT_ID", "CODEX_HOME"} {
		if !slices.Contains(scrubEnv, name) {
			t.Errorf("scrubEnv is missing %s", name)
		}
	}
}
