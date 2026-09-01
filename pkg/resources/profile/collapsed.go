package profile

import (
	"fmt"
	"sort"
	"strings"
)

// ToCollapsed renders the Brendan Gregg collapsed stack format grouped by thread state.
// Each section is sorted by that state's count descending; top > 0 limits each section to N entries.
// appOnly strips non-com.dynatrace frames from each path.
// Returns empty string for unsupported kinds or missing data.
func ToCollapsed(kind string, raw interface{}, top int, appOnly bool) string {
	switch kind {
	case "memoryAllocation", "memoryAllocationDetails":
		return memoryCollapsed(raw, top, appOnly)
	case "methodHotspots", "threadAnalysis":
		// handled below
	default:
		return ""
	}
	envelope, ok := raw.(map[string]interface{})
	if !ok {
		return ""
	}
	result, _ := envelope["result"].(map[string]interface{})
	nodes := dataNodes(result)
	if len(nodes) == 0 {
		return ""
	}

	rootID := nodeIDStr(result["dataRootNodeId"])

	type node struct {
		label    string
		samples  map[string]int
		children []string
	}

	nodeMap := make(map[string]*node, len(nodes))
	for _, n := range nodes {
		nm, ok := n.(map[string]interface{})
		if !ok {
			continue
		}
		id := nodeIDStr(nm["id"])
		nd := &node{
			label:   nodeLabel(nm),
			samples: nodeSamples(nm),
		}
		if cids, ok := nm["childIds"].([]interface{}); ok {
			for _, c := range cids {
				if s, ok := c.(string); ok {
					nd.children = append(nd.children, s)
				}
			}
		}
		nodeMap[id] = nd
	}

	type line struct {
		path    string
		samples map[string]int
	}
	var all []line

	var dfs func(id string, stack []string)
	dfs = func(id string, stack []string) {
		n := nodeMap[id]
		if n == nil {
			return
		}
		path := stack
		if n.label != "" && (!appOnly || strings.HasPrefix(n.label, appPrefix)) {
			path = append(stack, n.label)
		}
		if len(n.children) == 0 && len(path) > 0 {
			all = append(all, line{strings.Join(path, ";"), n.samples})
		}
		for _, cid := range n.children {
			dfs(cid, path)
		}
	}

	dfs(rootID, nil)
	if len(all) == 0 {
		return ""
	}

	var sb strings.Builder

	if top == 0 {
		sort.SliceStable(all, func(i, j int) bool {
			return all[i].samples["RUNNING"] > all[j].samples["RUNNING"]
		})
		for _, l := range all {
			fmt.Fprintf(&sb, "%s running=%d lock=%d net_io=%d disk_io=%d wait=%d\n",
				l.path, l.samples["RUNNING"], l.samples["LOCK"],
				l.samples["NET_IO"], l.samples["DISK_IO"], l.samples["WAIT"],
			)
		}
		return sb.String()
	}

	stateKeys := []string{"RUNNING", "LOCK", "NET_IO", "DISK_IO", "WAIT"}
	for i, key := range stateKeys {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sorted := make([]line, len(all))
		copy(sorted, all)
		sort.SliceStable(sorted, func(i, j int) bool {
			return sorted[i].samples[key] > sorted[j].samples[key]
		})
		if sorted[0].samples[key] == 0 {
			fmt.Fprintf(&sb, "# %s — no activity\n", key)
			continue
		}
		if len(sorted) > top {
			sorted = sorted[:top]
		}
		fmt.Fprintf(&sb, "# %s\n", key)
		for _, l := range sorted {
			if l.samples[key] == 0 {
				break
			}
			fmt.Fprintf(&sb, "%s running=%d lock=%d net_io=%d disk_io=%d wait=%d\n",
				l.path, l.samples["RUNNING"], l.samples["LOCK"],
				l.samples["NET_IO"], l.samples["DISK_IO"], l.samples["WAIT"],
			)
		}
	}
	return sb.String()
}

// memoryCollapsed folds a memory allocation result into Brendan-Gregg-style leaf
// stacks weighted by bytes, sorted by allocation size:
//
//	frame1;frame2;...;frameN alloc=<bytes> surv=<bytes> alloc_count=<n> surv_count=<n>
//
// It walks the flat call tree in analysisResult.stacktreeNodes (nodes keyed by id,
// linked via childIds, rooted at stacktreeRootIds). Note: analysisResult also carries
// an aggregated apiStacktreeNodes with no children — do NOT use that one.
//
// Orientation gotcha: this tree is INVERTED relative to the hotspots tree — the
// allocation-site method is the ROOT and its callers are the descendants. So the
// emitted path reads allocation-site-first (site;caller;...;outermost), and per-method
// totals live on the roots, not the leaves. Aggregating by leaf here gives Thread.run.
// alloc/surv are allocation pressure and GC-survivors within the window, not live
// retained heap. appOnly keeps only com.dynatrace.* frames in each path; top limits rows.
func memoryCollapsed(raw interface{}, top int, appOnly bool) string {
	envelope, ok := raw.(map[string]interface{})
	if !ok {
		return ""
	}
	result, _ := envelope["result"].(map[string]interface{})
	ar, _ := result["analysisResult"].(map[string]interface{})
	if ar == nil {
		return ""
	}
	nodesRaw, _ := ar["stacktreeNodes"].([]interface{})
	rootIDs, _ := ar["stacktreeRootIds"].([]interface{})
	if len(nodesRaw) == 0 || len(rootIDs) == 0 {
		return ""
	}

	type mnode struct {
		name     string
		ai       map[string]interface{}
		children []string
	}
	nodeMap := make(map[string]*mnode, len(nodesRaw))
	for _, r := range nodesRaw {
		nm, ok := r.(map[string]interface{})
		if !ok {
			continue
		}
		md := &mnode{name: strVal(nm, "name")}
		md.ai, _ = nm["allocationInfo"].(map[string]interface{})
		if cids, ok := nm["childIds"].([]interface{}); ok {
			for _, c := range cids {
				md.children = append(md.children, nodeIDStr(c))
			}
		}
		nodeMap[nodeIDStr(nm["id"])] = md
	}

	type line struct {
		path                  string
		allocSize, survSize   int
		allocCount, survCount int
	}
	var all []line

	var dfs func(id string, stack []string)
	dfs = func(id string, stack []string) {
		n := nodeMap[id]
		if n == nil {
			return
		}
		path := stack
		if n.name != "" && (!appOnly || strings.HasPrefix(n.name, appPrefix)) {
			path = append(stack, n.name)
		}
		if len(n.children) == 0 {
			if len(path) == 0 {
				return
			}
			all = append(all, line{
				path:       strings.Join(path, ";"),
				allocSize:  intVal(n.ai, "allocationSize"),
				survSize:   intVal(n.ai, "survivorSize"),
				allocCount: intVal(n.ai, "allocationCount"),
				survCount:  intVal(n.ai, "survivorCount"),
			})
			return
		}
		for _, cid := range n.children {
			dfs(cid, path)
		}
	}
	for _, rid := range rootIDs {
		dfs(nodeIDStr(rid), nil)
	}
	if len(all) == 0 {
		return ""
	}

	sort.SliceStable(all, func(i, j int) bool { return all[i].allocSize > all[j].allocSize })
	if top > 0 && len(all) > top {
		all = all[:top]
	}

	var sb strings.Builder
	sb.WriteString("# ALLOCATIONS (bytes, sorted by alloc; surv = survived >=1 GC in window, not live heap)\n")
	for _, l := range all {
		fmt.Fprintf(&sb, "%s alloc=%d surv=%d alloc_count=%d surv_count=%d\n",
			l.path, l.allocSize, l.survSize, l.allocCount, l.survCount)
	}
	return sb.String()
}
