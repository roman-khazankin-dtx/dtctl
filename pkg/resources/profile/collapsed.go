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
	if kind != "methodHotspots" && kind != "threadAnalysis" {
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
