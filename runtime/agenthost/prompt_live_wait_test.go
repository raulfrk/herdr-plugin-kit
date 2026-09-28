package agenthost

import (
	"encoding/json"
	"testing"
)

func TestCodexLivePromptWaitRejectsEchoAndUnfinishedTurn(t *testing.T) {
	for _, test := range []struct {
		name, text string
		want       bool
	}{
		{"submitted prompt", "› Reply exactly TOKEN", false},
		{"draft", "› TOKEN", false},
		{"embedded token", "• Not TOKEN yet", false},
		{"complete reply", "› Reply exactly TOKEN\n\n• TOKEN\n", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := livePromptReplyVisible("codex", test.text, "TOKEN"); got != test.want {
				t.Fatalf("visible=%t want=%t", got, test.want)
			}
		})
	}
	for _, test := range []struct {
		name, id, status, kind, text string
		want                         bool
	}{
		{"complete", "exact", "completed", "agentMessage", "TOKEN", true},
		{"prompt only", "exact", "completed", "userMessage", "TOKEN", false},
		{"in progress", "exact", "inProgress", "agentMessage", "TOKEN", false},
		{"interrupted", "exact", "interrupted", "agentMessage", "TOKEN", false},
		{"wrong thread", "other", "completed", "agentMessage", "TOKEN", false},
		{"wrong answer", "exact", "completed", "agentMessage", "TOKEN extra", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			data, err := json.Marshal(map[string]any{"thread": map[string]any{"id": test.id, "turns": []any{map[string]any{"status": test.status, "items": []any{map[string]string{"type": test.kind, "text": test.text}}}}}})
			if err != nil {
				t.Fatal(err)
			}
			var reply codexLiveReply
			if err := json.Unmarshal(data, &reply); err != nil {
				t.Fatal(err)
			}
			if got := reply.completed("exact", "TOKEN"); got != test.want {
				t.Fatalf("completed=%t want=%t", got, test.want)
			}
		})
	}
}
