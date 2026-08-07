package transcript

// finishTurns runs the harness-independent post-processing every parser needs:
// results paired onto their calls, empty turns dropped, same-role runs merged.
func finishTurns(turns []Turn) []Turn {
	turns = pairToolResults(turns)
	turns = removeEmptyTurns(turns)
	return mergeConsecutiveTurns(turns)
}

// pairToolResults attaches result blocks to their matching tool calls. Paired
// results move onto the call's Result field and leave the block list; results
// with no matching call remain as KindToolResult blocks.
func pairToolResults(turns []Turn) []Turn {
	callIndex := make(map[string]*ToolCall)
	for i := range turns {
		for j := range turns[i].Blocks {
			if tc := turns[i].Blocks[j].Tool; tc != nil && tc.ID != "" {
				callIndex[tc.ID] = tc
			}
		}
	}

	for i := range turns {
		filtered := turns[i].Blocks[:0]
		for _, b := range turns[i].Blocks {
			if b.Kind == KindToolResult && b.Result != nil && b.Result.CallID != "" {
				if call, ok := callIndex[b.Result.CallID]; ok {
					call.Result = b.Result
					continue
				}
			}
			filtered = append(filtered, b)
		}
		turns[i].Blocks = filtered
	}
	return turns
}

// mergeConsecutiveTurns combines adjacent turns with the same role into one turn.
func mergeConsecutiveTurns(turns []Turn) []Turn {
	if len(turns) == 0 {
		return turns
	}
	merged := []Turn{turns[0]}
	for i := 1; i < len(turns); i++ {
		last := &merged[len(merged)-1]
		if turns[i].Role == last.Role {
			last.Blocks = append(last.Blocks, turns[i].Blocks...)
		} else {
			merged = append(merged, turns[i])
		}
	}
	return merged
}

// removeEmptyTurns filters out turns with no content blocks.
func removeEmptyTurns(turns []Turn) []Turn {
	filtered := turns[:0]
	for _, t := range turns {
		if len(t.Blocks) > 0 {
			filtered = append(filtered, t)
		}
	}
	return filtered
}
