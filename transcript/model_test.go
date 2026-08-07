package transcript

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// TestSessionJSONRoundTrip is the contract that keeps the model honest: it
// must survive Marshal → Unmarshal unchanged, since serialized sessions are
// the interchange format for external consumers.
func TestSessionJSONRoundTrip(t *testing.T) {
	sess := &Session{
		Version: Version,
		ID:      "019fc7d2-71ee-7da3-8497-cf2834825a90",
		Harness: HarnessPi,
		CWD:     "/tmp/proj",
		Turns: []Turn{
			{
				Role:      "user",
				Timestamp: time.Date(2026, 8, 3, 13, 31, 35, 0, time.UTC),
				Blocks:    []Block{{Kind: KindText, Text: "fix the bug"}},
			},
			{
				Role:  "assistant",
				Model: "gpt-5.6-sol",
				Blocks: []Block{
					{Kind: KindThinking, Text: "hmm"},
					{Kind: KindToolCall, Tool: &ToolCall{
						ID:       "call_1",
						Name:     "Edit",
						RawName:  "edit",
						Input:    map[string]any{"file_path": "a.go", "old_string": "a", "new_string": "b"},
						RawInput: json.RawMessage(`{"path":"a.go"}`),
						Result:   &ToolResult{CallID: "call_1", Text: "ok"},
					}},
				},
			},
			{
				Role: "user",
				Blocks: []Block{{Kind: KindToolResult, Result: &ToolResult{
					CallID:  "orphan",
					Text:    "stale",
					IsError: true,
					Images:  []Image{{MediaType: "image/png", Data: "AAAA"}},
				}}},
			},
		},
	}

	data, err := json.Marshal(sess)
	if err != nil {
		t.Fatal(err)
	}
	var got Session
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sess, &got) {
		t.Errorf("round-trip mismatch:\nbefore: %+v\nafter:  %+v", sess, &got)
	}
}
