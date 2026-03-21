package main

import (
	"testing"
	"time"
)

func TestAssignSessionsToDays_SortsByDateDescending(t *testing.T) {
	now := time.Now()
	threeDaysAgo := now.AddDate(0, 0, -3)
	fiveDaysAgo := now.AddDate(0, 0, -5)

	// Feed sessions in wrong order: old, older, today
	sessions := []timelineSession{
		{SessionID: "old", newestTime: threeDaysAgo},
		{SessionID: "older", newestTime: fiveDaysAgo},
		{SessionID: "today", newestTime: now},
	}

	groups := assignSessionsToDays(sessions)

	if len(groups) != 3 {
		t.Fatalf("expected 3 day groups, got %d", len(groups))
	}
	if groups[0].Label != "Today" {
		t.Errorf("first group should be Today, got %q", groups[0].Label)
	}
	// Verify descending order: each group's time should be after the next
	for i := 0; i < len(groups)-1; i++ {
		a := groups[i].Sessions[0].newestTime
		b := groups[i+1].Sessions[0].newestTime
		if a.Before(b) {
			t.Errorf("group %d (%s) is older than group %d (%s)", i, groups[i].Label, i+1, groups[i+1].Label)
		}
	}
}

func TestAssignSessionsToDays_MultipleSessions_SameDay(t *testing.T) {
	// Use midday to avoid crossing midnight when subtracting
	now := time.Now()
	midday := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, now.Location())
	sessions := []timelineSession{
		{SessionID: "a", newestTime: midday.Add(-1 * time.Hour)},
		{SessionID: "b", newestTime: midday.Add(-2 * time.Hour)},
	}

	groups := assignSessionsToDays(sessions)

	if len(groups) != 1 {
		t.Fatalf("expected 1 day group, got %d", len(groups))
	}
	if len(groups[0].Sessions) != 2 {
		t.Errorf("expected 2 sessions in group, got %d", len(groups[0].Sessions))
	}
}
