package main

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// lcsLen is the reference: a minimal diff changes len(a)+len(b)-2*lcs lines.
func lcsLen(a, b []string) int {
	dp := make([][]int, len(a)+1)
	for i := range dp {
		dp[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				dp[i][j] = max(dp[i+1][j], dp[i][j+1])
			}
		}
	}
	return dp[0][0]
}

// sides rebuilds both inputs from ops.
func sides(ops []diffOp) (a, b []string, changed int) {
	for _, op := range ops {
		if op.kind != '+' {
			a = append(a, op.text)
		}
		if op.kind != '-' {
			b = append(b, op.text)
		}
		if op.kind != ' ' {
			changed++
		}
	}
	return a, b, changed
}

func toKinds(ops []diffOp) string {
	b := make([]byte, len(ops))
	for i, op := range ops {
		b[i] = op.kind
	}
	return string(b)
}

func TestDiffOpsMinimal(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	gen := func() []string {
		s := make([]string, rng.Intn(25))
		for i := range s {
			s[i] = string(rune('a' + rng.Intn(4)))
		}
		return s
	}
	for i := 0; i < 3000; i++ {
		a, b := gen(), gen()
		ra, rb, changed := sides(diffOps(a, b))
		if strings.Join(ra, ",") != strings.Join(a, ",") || strings.Join(rb, ",") != strings.Join(b, ",") {
			t.Fatalf("ops do not rebuild inputs\na=%v\nb=%v", a, b)
		}
		if ops := diffOps(a, b); strings.Contains(toKinds(ops), "+-") {
			t.Fatalf("addition before removal in one change run: %q\na=%v\nb=%v", toKinds(ops), a, b)
		}
		if want := len(a) + len(b) - 2*lcsLen(a, b); changed != want {
			t.Fatalf("changed %d lines, minimal is %d\na=%v\nb=%v", changed, want, a, b)
		}
	}
}

func numbered(n int, prefix string) []string {
	s := make([]string, n)
	for i := range s {
		s[i] = fmt.Sprintf("%s%d", prefix, i)
	}
	return s
}

func TestDiffOpsLargeFiles(t *testing.T) {
	// Edits at both ends of a long file: the old LCS table gave up here and
	// rendered the whole file as removed and re-added.
	a := numbered(6000, "line ")
	b := append([]string{"new top"}, a...)
	b[3000] = "changed middle"
	b = append(b, "new bottom")
	if _, _, changed := sides(diffOps(a, b)); changed != 4 {
		t.Errorf("changed = %d, want 4", changed)
	}

	start := time.Now()
	ra, rb, _ := sides(diffOps(numbered(20000, "old "), numbered(20000, "new ")))
	if len(ra) != 20000 || len(rb) != 20000 {
		t.Errorf("rewrite did not rebuild inputs: %d/%d lines", len(ra), len(rb))
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("full rewrite took %v; maxDiffDepth should bound it", d)
	}
}

func TestHunkDiffHTML(t *testing.T) {
	a := numbered(20, "l")
	b := append([]string{}, a...)
	b[1] = "x"
	b[15] = "y"
	html, added, removed := hunkDiffHTML(diffOps(a, b), 1000)
	if added != 2 || removed != 2 {
		t.Errorf("added/removed = %d/%d, want 2/2", added, removed)
	}
	for _, want := range []string{"@@ -1,5 +1,5 @@", "@@ -13,7 +13,7 @@", `<span class="diff-ln">2</span>+ x`, `<span class="diff-ln"></span>- l1`} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q in\n%s", want, html)
		}
	}
	if strings.Contains(html, ">l8<") || strings.Contains(html, "  l8") {
		t.Errorf("line 9 is outside every hunk's context:\n%s", html)
	}

	html, _, _ = hunkDiffHTML(diffOps(nil, numbered(10, "n")), 4)
	if !strings.Contains(html, "@@ -0,0 +1,10 @@") || !strings.Contains(html, "6 more lines") {
		t.Errorf("created file header or cap note wrong:\n%s", html)
	}
}
