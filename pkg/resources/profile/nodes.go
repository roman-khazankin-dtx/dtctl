package profile

import "fmt"

// Node accessors for the API's render tree, shared by the folded-stack renderers.

func dataNodes(result map[string]interface{}) []interface{} {
	if result == nil {
		return nil
	}
	nodes, _ := result["dataNodes"].([]interface{})
	return nodes
}

// nodeIsApp reports whether the profiler classified this frame as the application's
// own code. The API flag is counterintuitively named: apiInfo.systemApi == true means
// application code; false means built-in/library frames (JRE, Spring, Apache) AND the
// Dynatrace OneAgent itself (com.dynatrace.agent.*) — exactly the noise --app-only drops.
//
// Caveat: the classification is the API's, and it is not uniform across languages.
// On .NET the OneAgent's own frames (Dynatrace.OneAgent.*) are flagged systemApi==true,
// so --app-only keeps them; on Java the agent frames are correctly excluded. We take
// the flag as-is rather than second-guessing it per language.
func nodeIsApp(n map[string]interface{}) bool {
	ai, _ := n["apiInfo"].(map[string]interface{})
	if ai == nil {
		return false
	}
	b, _ := ai["systemApi"].(bool)
	return b
}

func nodeLabel(n map[string]interface{}) string {
	cp, _ := n["classPath"].(string)
	cn, _ := n["className"].(string)
	mn, _ := n["methodName"].(string)
	if cn == "" || mn == "" {
		return ""
	}
	if cp != "" {
		return cp + "." + cn + "." + mn
	}
	return cn + "." + mn
}

func nodeSamples(n map[string]interface{}) map[string]int {
	out := map[string]int{"RUNNING": 0, "LOCK": 0, "NET_IO": 0, "DISK_IO": 0, "WAIT": 0}
	s, ok := n["samples"].(map[string]interface{})
	if !ok {
		return out
	}
	for k := range out {
		if v, ok := s[k].(float64); ok {
			out[k] = int(v)
		}
	}
	return out
}

func nodeIDStr(v interface{}) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return fmt.Sprintf("%g", x)
	}
	return ""
}

func strVal(m map[string]interface{}, key string) string {
	v, _ := m[key].(string)
	return v
}

func intVal(m map[string]interface{}, key string) int {
	v, _ := m[key].(float64)
	return int(v)
}
