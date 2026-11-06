package main

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

type diffOp struct {
	kind byte // ' ' context, '-' removed, '+' added
	text string
}

// maxDiffDepth bounds the edit distance one split searches before the
// region renders as remove-then-add, so rewriting a huge file cannot stall a
// request.
const maxDiffDepth = 2000

// diffOps computes a minimal line diff (Myers, linear-space variant),
// keeping unchanged lines as context.
func diffOps(a, b []string) []diffOp {
	ids := make(map[string]int, len(a)+len(b))
	d := differ{a: a, b: b, x: lineIDs(a, ids), y: lineIDs(b, ids)}
	d.diff(0, len(a), 0, len(b))
	return removalsFirst(d.ops)
}

// removalsFirst orders each run of changes as removals, then additions. The
// search may emit them interleaved; both orders are minimal.
func removalsFirst(ops []diffOp) []diffOp {
	for i := 0; i < len(ops); {
		j := i
		for j < len(ops) && ops[j].kind != ' ' {
			j++
		}
		slices.SortStableFunc(ops[i:j], func(p, q diffOp) int { return cmp.Compare(q.kind, p.kind) }) // '-' > '+'
		i = j + 1
	}
	return ops
}

// lineIDs interns lines so the search compares ints, not strings.
func lineIDs(lines []string, ids map[string]int) []int {
	out := make([]int, len(lines))
	for i, l := range lines {
		id, ok := ids[l]
		if !ok {
			id = len(ids)
			ids[l] = id
		}
		out[i] = id
	}
	return out
}

type differ struct {
	a, b []string
	x, y []int // line ids of a and b
	ops  []diffOp
}

// diff appends the ops that turn a[a0:a1] into b[b0:b1].
func (d *differ) diff(a0, a1, b0, b1 int) {
	for a0 < a1 && b0 < b1 && d.x[a0] == d.y[b0] {
		d.ops = append(d.ops, diffOp{' ', d.a[a0]})
		a0++
		b0++
	}
	end := a1
	for a1 > a0 && b1 > b0 && d.x[a1-1] == d.y[b1-1] {
		a1--
		b1--
	}
	if sa, sb, ok := d.split(a0, a1, b0, b1); ok {
		d.diff(a0, sa, b0, sb)
		d.diff(sa, a1, sb, b1)
	} else {
		d.ops = append(d.ops, flatDiff(d.a[a0:a1], d.b[b0:b1])...)
	}
	for _, l := range d.a[a1:end] {
		d.ops = append(d.ops, diffOp{' ', l})
	}
}

// split finds a point on a shortest edit path through the middle of the
// region. ok is false when one side is empty or the search exceeds
// maxDiffDepth.
func (d *differ) split(a0, a1, b0, b1 int) (int, int, bool) {
	x, y := d.x[a0:a1], d.y[b0:b1]
	if len(x) == 0 || len(y) == 0 {
		return 0, 0, false
	}
	px, py, ok := newMiddleSearch(x, y).find()
	return a0 + px, b0 + py, ok
}

// middleSearch extends paths forward from (0,0) and backward from (n,m)
// until they overlap. fwd[off+k] and bwd[off+k] hold the furthest x each
// direction reached on diagonal k = x-y (backward in reversed coordinates).
type middleSearch struct {
	x, y       []int
	off, delta int
	fwd, bwd   []int
}

func newMiddleSearch(x, y []int) *middleSearch {
	maxD := min((len(x)+len(y)+1)/2, maxDiffDepth)
	s := &middleSearch{x: x, y: y, off: maxD + 1, delta: len(x) - len(y),
		fwd: make([]int, 2*maxD+3), bwd: make([]int, 2*maxD+3)}
	for i := range s.fwd {
		s.fwd[i], s.bwd[i] = -1, -1
	}
	s.fwd[s.off+1], s.bwd[s.off+1] = 0, 0
	return s
}

// find alternates one forward and one backward step per depth. An odd delta
// lets the paths meet only on a forward step, an even one on a backward step.
func (s *middleSearch) find() (int, int, bool) {
	odd := s.delta%2 != 0
	var fLo, fHi, bLo, bHi int // diagonals whose path left the grid
	for depth := 0; depth < s.off-1; depth++ {
		if px, py, ok := s.step(depth, true, &fLo, &fHi, odd); ok {
			return s.splitPoint(px, py)
		}
		if px, py, ok := s.step(depth, false, &bLo, &bHi, !odd); ok {
			return s.splitPoint(px, py)
		}
	}
	return 0, 0, false
}

// step extends every path of one direction by one edit plus its snake.
// With check set it returns the meeting point once a path overlaps the
// other direction's path on the same diagonal.
func (s *middleSearch) step(depth int, forward bool, lo, hi *int, check bool) (int, int, bool) {
	n, m := len(s.x), len(s.y)
	v, other := s.fwd, s.bwd
	if !forward {
		v, other = s.bwd, s.fwd
	}
	for k := -depth + *lo; k <= depth-*hi; k += 2 {
		px := furthest(v, s.off+k, k == -depth, k == depth)
		py := px - k
		for px < n && py < m && s.same(forward, px, py) {
			px++
			py++
		}
		v[s.off+k] = px
		switch {
		case px > n:
			*hi += 2
		case py > m:
			*lo += 2
		case check:
			if j, ok := s.overlap(other, k, px); ok {
				if forward {
					return px, py, true
				}
				return other[j], other[j] - (j - s.off), true
			}
		}
	}
	return 0, 0, false
}

// overlap reports whether a path at px on diagonal k reaches the other
// direction's furthest path on the matching diagonal j.
func (s *middleSearch) overlap(other []int, k, px int) (j int, ok bool) {
	j = s.off + s.delta - k
	return j, j >= 0 && j < len(other) && other[j] != -1 && px+other[j] >= len(s.x)
}

func (s *middleSearch) same(forward bool, px, py int) bool {
	if forward {
		return s.x[px] == s.y[py]
	}
	return s.x[len(s.x)-px-1] == s.y[len(s.y)-py-1]
}

// splitPoint rejects a corner: splitting there would not shrink the region.
func (s *middleSearch) splitPoint(px, py int) (int, int, bool) {
	corner := (px == 0 && py == 0) || (px == len(s.x) && py == len(s.y))
	return px, py, !corner
}

// furthest picks the start of diagonal i's next path: down from diagonal
// i+1, or right from i-1, whichever reached further.
func furthest(v []int, i int, lowest, highest bool) int {
	if lowest || (!highest && v[i-1] < v[i+1]) {
		return v[i+1]
	}
	return v[i-1] + 1
}

// flatDiff renders every line of a as removed then every line of b as added.
func flatDiff(a, b []string) []diffOp {
	ops := make([]diffOp, 0, len(a)+len(b))
	for _, l := range a {
		ops = append(ops, diffOp{'-', l})
	}
	for _, l := range b {
		ops = append(ops, diffOp{'+', l})
	}
	return ops
}

// diffContext is the number of unchanged lines framing each hunk.
const diffContext = 3

type hunkSpan struct{ lo, hi int } // op index range

// diffHunks groups changed ops with their context, merging hunks that touch.
func diffHunks(ops []diffOp) (hunks []hunkSpan, added, removed int) {
	for i, op := range ops {
		switch op.kind {
		case '+':
			added++
		case '-':
			removed++
		default:
			continue
		}
		lo, hi := max(i-diffContext, 0), min(i+diffContext+1, len(ops))
		if n := len(hunks); n > 0 && lo <= hunks[n-1].hi {
			hunks[n-1].hi = hi
		} else {
			hunks = append(hunks, hunkSpan{lo, hi})
		}
	}
	return hunks, added, removed
}

// hunkDiffHTML renders ops as unified hunks with new-file line numbers. At
// most maxLines lines render; added and removed count every change.
func hunkDiffHTML(ops []diffOp, maxLines int) (html string, added, removed int) {
	hunks, added, removed := diffHunks(ops)
	total := 0
	for _, h := range hunks {
		total += h.hi - h.lo
	}
	var b strings.Builder
	b.WriteString(`<pre class="transcript-mini-diff changes-diff">`)
	oldNo, newNo, pos, rendered := 1, 1, 0, 0
	advance := func(op diffOp) {
		if op.kind != '+' {
			oldNo++
		}
		if op.kind != '-' {
			newNo++
		}
	}
	for _, h := range hunks {
		for ; pos < h.lo; pos++ {
			advance(ops[pos])
		}
		if rendered >= maxLines {
			break
		}
		b.WriteString(hunkHeader(ops[h.lo:h.hi], oldNo, newNo))
		for ; pos < h.hi && rendered < maxLines; pos++ {
			op := ops[pos]
			ln := ""
			if op.kind != '-' {
				ln = fmt.Sprint(newNo)
			}
			class, sign := "diff-context", "  "
			switch op.kind {
			case '+':
				class, sign = "diff-add", "+ "
			case '-':
				class, sign = "diff-remove", "- "
			}
			b.WriteString(`<span class="` + class + `"><span class="diff-ln">` + ln + `</span>` + sign + esc(op.text) + `</span>`)
			advance(op)
			rendered++
		}
	}
	if rest := total - rendered; rest > 0 {
		fmt.Fprintf(&b, `<span class="diff-meta">… %d more %s</span>`, rest, plural(rest, "line"))
	}
	b.WriteString(`</pre>`)
	return b.String(), added, removed
}

// hunkHeader writes a unified "@@ -old +new @@" header; an empty side
// starts at the line before, as git prints it.
func hunkHeader(ops []diffOp, oldNo, newNo int) string {
	oldLen, newLen := 0, 0
	for _, op := range ops {
		if op.kind != '+' {
			oldLen++
		}
		if op.kind != '-' {
			newLen++
		}
	}
	if oldLen == 0 {
		oldNo--
	}
	if newLen == 0 {
		newNo--
	}
	return fmt.Sprintf(`<span class="diff-meta">@@ -%d,%d +%d,%d @@</span>`, oldNo, oldLen, newNo, newLen)
}
