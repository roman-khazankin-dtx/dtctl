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

// Interior nodes carry self samples; a naive leaf-only fold would drop them.
// Every node's self time must be attributed, so emitted RUNNING sums to the
// true total across all nodes.
func TestToCollapsed_EmitsInteriorSelfSamples(t *testing.T) {
	// root(0) -> A(running=10, has child) -> B(leaf, running=3)
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
		// The interior frame A must appear as its own stack (self time), not only as a prefix of B.
		if l == "com.ex.Foo.a running=10 lock=0 net_io=0 disk_io=0 wait=0" {
			sawInterior = true
		}
	}
	if total != 13 {
		t.Fatalf("emitted RUNNING = %d, want 13 (interior self time must not be dropped)\n%s", total, out)
	}
	if !sawInterior {
		t.Fatalf("interior frame A's self samples were not emitted as their own line:\n%s", out)
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
