package engine

import (
	"reflect"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

// A Claude Code OAuth token reaches only the claude CLI, as
// CLAUDE_CODE_OAUTH_TOKEN, never as an API key.
func TestHarnessAuthEnvOAuthToken(t *testing.T) {
	got := HarnessAuthEnv(core.HarnessClaude, "", core.SecretAnthropicOAuth, []byte(" tok\n"))
	if want := map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "tok"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("claude: got %v, want %v", got, want)
	}
	for _, h := range []core.HarnessType{core.HarnessCodex, core.HarnessType("open")} {
		if got := HarnessAuthEnv(h, "", core.SecretAnthropicOAuth, []byte("tok")); len(got) != 0 {
			t.Errorf("%s: got %v, want no variables", h, got)
		}
	}
}
