package profile

import (
	"strings"
	"testing"
)

// node is a small helper to build a dataNodes entry.
func hnode(id string, cls, method string, app bool, running, lock int, children ...string) map[string]interface{} {
	kids := make([]interface{}, len(children))
	for i, c := range children {
		kids[i] = c
	}
	return map[string]interface{}{
		"id":         id,
		"classPath":  "",
		"className":  cls,
		"methodName": method,
		"apiInfo":    map[string]interface{}{"systemApi": app},
		"samples":    map[string]interface{}{"RUNNING": float64(running), "LOCK": float64(lock)},
		"childIds":   kids,
	}
}

func envelope(rootID string, nodes ...map[string]interface{}) map[string]interface{} {
	arr := make([]interface{}, len(nodes))
	for i, n := range nodes {
		arr[i] = n
	}
	return map[string]interface{}{
		"result": map[string]interface{}{
			"dataRootNodeId": rootID,
			"dataNodes":      arr,
		},
	}
}

func runOf(t *testing.T, line string) int {
	t.Helper()
	i := strings.Index(line, "running=")
	if i < 0 {
		t.Fatalf("no running= in %q", line)
	}
	rest := line[i+len("running="):]
	if sp := strings.IndexByte(rest, ' '); sp >= 0 {
		rest = rest[:sp]
	}
	n := 0
	for _, c := range rest {
		n = n*10 + int(c-'0')
	}
	return n
}

// API counts are cumulative: A's running=10 already includes B's running=3, so
// A's own (self) cost is 10-3=7. Interior self time must be attributed (a
// leaf-only fold drops it entirely), and because self = node - Σ children the
// emitted RUNNING telescopes back to the root total (10), never double-counts.
func TestToCollapsed_EmitsInteriorSelfSamples(t *testing.T) {
	// root(0, cumulative=10) -> A(cumulative running=10, has child) -> B(leaf, running=3)
	raw := envelope("0",
		hnode("0", "", "", false, 0, 0, "A"),
		hnode("A", "com.ex.Foo", "a", true, 10, 0, "B"),
		hnode("B", "com.ex.Foo", "b", true, 3, 0),
	)
	out := ToCollapsed("methodHotspots", raw, 0, false)

	var total int
	var sawInterior bool
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l == "" {
			continue
		}
		total += runOf(t, l)
		// Interior frame A must appear as its own stack carrying its SELF time
		// (10 cumulative - 3 in child B = 7), not its cumulative 10.
		if l == "com.ex.Foo.a running=7 lock=0 net_io=0 disk_io=0 wait=0" {
			sawInterior = true
		}
	}
	if total != 10 {
		t.Fatalf("emitted RUNNING = %d, want 10 (self counts telescope to the root total, no double-count)\n%s", total, out)
	}
	if !sawInterior {
		t.Fatalf("interior frame A's self samples (running=7) were not emitted as their own line:\n%s", out)
	}
}

// --app-only strips non-app frames, which can collapse distinct subtrees onto
// the same application stack. Those must be merged into one line with summed
// counts, not printed as duplicates.
func TestToCollapsed_AppOnlyMergesCollidingStacks(t *testing.T) {
	// root -> App(app) -> Lib1(non-app, running=5), Lib2(non-app, running=7)
	// Under --app-only both children collapse to the single path "App".
	raw := envelope("0",
		hnode("0", "", "", false, 0, 0, "App"),
		hnode("App", "com.ex.Svc", "handle", true, 0, 0, "L1", "L2"),
		hnode("L1", "jdk.internal", "x", false, 5, 0),
		hnode("L2", "jdk.internal", "y", false, 7, 0),
	)
	out := strings.TrimSpace(ToCollapsed("methodHotspots", raw, 0, true))
	lines := strings.Split(out, "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 merged line, got %d:\n%s", len(lines), out)
	}
	if got := runOf(t, lines[0]); got != 12 {
		t.Fatalf("merged RUNNING = %d, want 12 (5+7 summed)\n%s", got, out)
	}
	if !strings.HasPrefix(lines[0], "com.ex.Svc.handle running=") {
		t.Fatalf("merged line should be the app path only, got:\n%s", out)
	}
}

// Self is computed per thread state (self = node - Σ children, each state
// independently). An interior node with several children keeps only what it did
// not delegate, and a pure pass-through (all of its cumulative count lives in
// its children) contributes no line at all.
func TestToCollapsed_SelfIsPerStateAndSkipsPassThrough(t *testing.T) {
	// root -> P(run=20,lock=5) -> X(run=8), Y(run=5,lock=2)
	//      -> Q(run=4)         -> Z(run=4)              [Q is a pure pass-through]
	raw := envelope("0",
		hnode("0", "", "", false, 0, 0, "P", "Q"),
		hnode("P", "com.ex.T", "p", true, 20, 5, "X", "Y"),
		hnode("X", "com.ex.T", "x", true, 8, 0),
		hnode("Y", "com.ex.T", "y", true, 5, 2),
		hnode("Q", "com.ex.T", "q", true, 4, 0, "Z"),
		hnode("Z", "com.ex.T", "z", true, 4, 0),
	)
	out := strings.TrimSpace(ToCollapsed("methodHotspots", raw, 0, false))

	var total int
	byLine := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		total += runOf(t, l)
		byLine[l] = true
	}
	// P kept run=20-(8+5)=7, lock=5-(0+2)=3.
	if !byLine["com.ex.T.p running=7 lock=3 net_io=0 disk_io=0 wait=0"] {
		t.Fatalf("P self must be per-state (run=7, lock=3):\n%s", out)
	}
	// Q delegated everything (4-4=0) — it must not appear as its own stack.
	if byLine["com.ex.T.q running=0 lock=0 net_io=0 disk_io=0 wait=0"] {
		t.Fatalf("pure pass-through Q must not emit a line:\n%s", out)
	}
	// Emitted RUNNING telescopes to the true total (P 20 + Q 4 = 24).
	if total != 24 {
		t.Fatalf("emitted RUNNING = %d, want 24\n%s", total, out)
	}
}

// A child reporting more of a state than its parent (sampling jitter) must clamp
// that state's self to zero, never emit a negative count.
func TestToCollapsed_ClampsNegativeSelf(t *testing.T) {
	// A: run=3, lock=0 with child B: run=1, lock=5 -> A self lock = 0-5, clamp 0.
	raw := envelope("0",
		hnode("0", "", "", false, 0, 0, "A"),
		hnode("A", "com.ex.C", "a", true, 3, 0, "B"),
		hnode("B", "com.ex.C", "b", true, 1, 5),
	)
	out := strings.TrimSpace(ToCollapsed("methodHotspots", raw, 0, false))
	if !strings.Contains(out, "com.ex.C.a running=2 lock=0 net_io=0 disk_io=0 wait=0") {
		t.Fatalf("A self must clamp negative lock to 0 (run=2, lock=0):\n%s", out)
	}
	if strings.Contains(out, "lock=-") {
		t.Fatalf("no negative counts allowed:\n%s", out)
	}
}

// The schema is one line per unique stack regardless of top; top only caps
// rows and never introduces per-state section headers.
func TestToCollapsed_TopCapsRowsNoSections(t *testing.T) {
	raw := envelope("0",
		hnode("0", "", "", false, 0, 0, "A", "B", "C"),
		hnode("A", "com.ex.C", "a", true, 9, 0),
		hnode("B", "com.ex.C", "b", true, 5, 1),
		hnode("C", "com.ex.C", "c", true, 1, 0),
	)
	out := strings.TrimSpace(ToCollapsed("methodHotspots", raw, 2, false))
	lines := strings.Split(out, "\n")
	if len(lines) != 2 {
		t.Fatalf("top=2 should emit 2 rows, got %d:\n%s", len(lines), out)
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "#") {
			t.Fatalf("output must not contain section headers:\n%s", out)
		}
	}
	// Sorted by RUNNING desc: A(9) then B(5).
	if !strings.HasPrefix(lines[0], "com.ex.C.a running=9") {
		t.Fatalf("rows not sorted by RUNNING desc:\n%s", out)
	}
}
