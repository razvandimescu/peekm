package transcript

import "testing"

func TestPairToolResults(t *testing.T) {
	t.Run("pairs matching call and result", func(t *testing.T) {
		turns := []Turn{
			{Role: "assistant", Blocks: []Block{
				{Kind: KindToolCall, Tool: &ToolCall{ID: "t1", Name: "Bash"}},
			}},
			{Role: "user", Blocks: []Block{
				{Kind: KindToolResult, Result: &ToolResult{CallID: "t1", Text: "output"}},
			}},
		}
		result := pairToolResults(turns)
		if result[0].Blocks[0].Tool.Result == nil {
			t.Error("tool call should have paired result")
		}
		if len(result[1].Blocks) != 0 {
			t.Errorf("tool result should be removed from user turn, got %d blocks", len(result[1].Blocks))
		}
	})

	t.Run("unmatched result stays", func(t *testing.T) {
		turns := []Turn{
			{Role: "user", Blocks: []Block{
				{Kind: KindToolResult, Result: &ToolResult{CallID: "orphan", Text: "no match"}},
			}},
		}
		result := pairToolResults(turns)
		if len(result[0].Blocks) != 1 {
			t.Error("unmatched tool result should remain")
		}
	})

	t.Run("empty turns", func(t *testing.T) {
		if result := pairToolResults(nil); len(result) != 0 {
			t.Errorf("expected empty, got %d turns", len(result))
		}
	})
}

func TestMergeConsecutiveTurns(t *testing.T) {
	t.Run("merges same role", func(t *testing.T) {
		turns := []Turn{
			{Role: "user", Blocks: []Block{{Kind: KindText, Text: "a"}}},
			{Role: "user", Blocks: []Block{{Kind: KindText, Text: "b"}}},
			{Role: "assistant", Blocks: []Block{{Kind: KindText, Text: "c"}}},
		}
		result := mergeConsecutiveTurns(turns)
		if len(result) != 2 {
			t.Fatalf("expected 2 turns, got %d", len(result))
		}
		if len(result[0].Blocks) != 2 {
			t.Errorf("first turn should have 2 blocks, got %d", len(result[0].Blocks))
		}
	})

	t.Run("no merge needed", func(t *testing.T) {
		turns := []Turn{
			{Role: "user", Blocks: []Block{{Kind: KindText}}},
			{Role: "assistant", Blocks: []Block{{Kind: KindText}}},
		}
		if result := mergeConsecutiveTurns(turns); len(result) != 2 {
			t.Fatalf("expected 2 turns, got %d", len(result))
		}
	})

	t.Run("empty input", func(t *testing.T) {
		if result := mergeConsecutiveTurns(nil); len(result) != 0 {
			t.Errorf("expected empty, got %d", len(result))
		}
	})
}

func TestRemoveEmptyTurns(t *testing.T) {
	turns := []Turn{
		{Role: "user", Blocks: []Block{{Kind: KindText}}},
		{Role: "assistant", Blocks: nil},
		{Role: "user", Blocks: []Block{}},
		{Role: "assistant", Blocks: []Block{{Kind: KindText}}},
	}
	result := removeEmptyTurns(turns)
	if len(result) != 2 {
		t.Fatalf("expected 2 non-empty turns, got %d", len(result))
	}
	if result[0].Role != "user" || result[1].Role != "assistant" {
		t.Errorf("unexpected roles: %s, %s", result[0].Role, result[1].Role)
	}
}
