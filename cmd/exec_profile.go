package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dynatrace-oss/dtctl/pkg/output"
	"github.com/dynatrace-oss/dtctl/pkg/resources/profile"
	"github.com/dynatrace-oss/dtctl/pkg/safety"
)

var execProfileCmd = &cobra.Command{
	Use:   "profile",
	Short: "Run a code-level profiling analysis on a Dynatrace entity",
	Long: `Run a code-level profiling analysis via the Dynatrace code-level analysis API.

The analysis is asynchronous server-side; this command polls until it completes.
Output is Brendan Gregg collapsed/folded stacks grouped by thread state:
  frame1;frame2;...;frameN running=16 lock=0 net_io=0 disk_io=0 wait=0

Entities: only PROCESS, PROCESS_GROUP, and PROCESS_GROUP_INSTANCE are eligible
— services are not accepted by the code-level analysis API. PROCESS is the
3rd-gen rename of PROCESS_GROUP_INSTANCE (same entity, same id suffix); pass a
PROCESS-xxx id straight from smartscapeNodes and dtctl rewrites it to the
classic PROCESS_GROUP_INSTANCE-xxx form the API expects.

Kinds:
  hotspots        Method hotspots for a PROCESS_GROUP or PROCESS_GROUP_INSTANCE
  threads         Thread analysis for a PROCESS_GROUP or PROCESS_GROUP_INSTANCE
  memory          Memory allocation analysis for a PROCESS_GROUP or PGI
  memory-details  Drill-down into a specific type/method (requires --type and --method)

memory/memory-details show where allocations happen and what survived GC within the
window — allocation pressure, not live retained heap. They don't prove a leak or
predict OOM; confirm that with a heap/RSS trend and pod restart/OOM state. The best
leak tell is survived/allocated: a frame whose survivors far exceed its in-window
allocations is retaining, not churning — that ratio, not raw allocation size, finds
the leak (the biggest allocator is usually just churn). NOTE the memory tree is
INVERTED vs hotspots: the allocation-site method is at the ROOT and its callers hang
below as children (the folded output already handles this; mind it if you parse JSON).

Samples are aggregated across the whole window (max 24h). Bound it with --from/--to
for a clean read.

Leaf-type filter (hotspots/threads) — pick by what you're chasing:
  --leaf-type service     samples taken inside a traced service/request context;
                          the one to reach for when localizing why a specific
                          endpoint or call path is hot (drops agent + ambient noise).
  --leaf-type background  ambient work outside any request (GC, JIT, schedulers, pools).
  --leaf-type total       (default) everything, including agent/framework overhead;
                          broadest view, but frame shares are diluted by non-app samples.

Examples:
  dtctl exec profile -k hotspots -e PROCESS-ABC123 --last 1h  # 3rd-gen id, rewritten to PGI
  dtctl exec profile -k hotspots -e PROCESS_GROUP_INSTANCE-ABC123 --last 1h
  dtctl exec profile -k hotspots -e PROCESS_GROUP_INSTANCE-ABC123 --last 1h --app-only
  dtctl exec profile -k hotspots -e PROCESS_GROUP-ABC123 --last 1h --leaf-type background
  dtctl exec profile -k hotspots -e PROCESS_GROUP-ABC123 --last 1h --leaf-type service --top 5
  dtctl exec profile -k threads -e PROCESS_GROUP_INSTANCE-ABC123 --last 30min
  dtctl exec profile -k memory -e PROCESS_GROUP-DEF456 --last 1h --survivors-only
  dtctl exec profile -k memory-details -e PROCESS_GROUP-DEF456 --last 1h \
    --type java.lang.String --method "java.lang.String.intern()"
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		kindShort, _ := cmd.Flags().GetString("kind")
		entityID, _ := cmd.Flags().GetString("entity")
		lastStr, _ := cmd.Flags().GetString("last")
		fromStr, _ := cmd.Flags().GetString("from")
		toStr, _ := cmd.Flags().GetString("to")

		apiKind, ok := profile.Kind[kindShort]
		if !ok {
			return fmt.Errorf("unknown kind %q: use hotspots, threads, memory, or memory-details", kindShort)
		}
		if entityID == "" {
			return fmt.Errorf("--entity is required")
		}

		var from, to int64
		if lastStr != "" {
			lastStr = strings.ReplaceAll(lastStr, "min", "m")
			last, err := time.ParseDuration(lastStr)
			if err != nil {
				return fmt.Errorf("invalid --last value %q: %w", lastStr, err)
			}
			now := time.Now()
			from, to = now.Add(-last).UnixMilli(), now.UnixMilli()
		} else {
			if fromStr == "" || toStr == "" {
				return fmt.Errorf("provide --last <duration> or both --from and --to")
			}
			var err error
			from, err = parseProfileTimestamp(fromStr)
			if err != nil {
				return err
			}
			to, err = parseProfileTimestamp(toStr)
			if err != nil {
				return err
			}
		}

		leafType, _ := cmd.Flags().GetString("leaf-type")
		showWaiting, _ := cmd.Flags().GetBool("show-waiting")
		problemID, _ := cmd.Flags().GetString("problem-id")
		survivorsOnly, _ := cmd.Flags().GetBool("survivors-only")
		typeFilter, _ := cmd.Flags().GetString("type-filter")
		apiFilter, _ := cmd.Flags().GetString("api-filter")
		methodFQNFilter, _ := cmd.Flags().GetString("method-fqn-filter")
		allocType, _ := cmd.Flags().GetString("type")
		method, _ := cmd.Flags().GetString("method")

		// Code-level analysis only reads profiling data — gate on OperationRead.
		_, c, err := SetupWithSafety(safety.OperationRead)
		if err != nil {
			return err
		}

		handler := profile.NewHandler(c)
		stopSpinner := startSpinner(fmt.Sprintf("running %s analysis", kindShort))
		resp, err := handler.Run(cmd.Context(), profile.Payload{
			Kind:            apiKind,
			EntityID:        entityID,
			From:            from,
			To:              to,
			LeafType:        leafType,
			ShowWaiting:     showWaiting,
			ProblemID:       problemID,
			SurvivorsOnly:   survivorsOnly,
			TypeFilter:      typeFilter,
			APIFilter:       apiFilter,
			MethodFQNFilter: methodFQNFilter,
			Type:            allocType,
			Method:          method,
		})
		stopSpinner()
		if err != nil {
			return err
		}

		// Marshal resp to a generic map so enrich/compact helpers see the
		// expected envelope shape: {"status":..., "result":{...}, ...}
		b, _ := json.Marshal(resp)
		var result interface{}
		_ = json.Unmarshal(b, &result)
		result = compactResult(result)

		outputFmt, _ := cmd.Flags().GetString("output")
		if outputFmt == "json" {
			return output.NewPrinter("json").Print(result)
		}

		appOnly, _ := cmd.Flags().GetBool("app-only")
		top, _ := cmd.Flags().GetInt("top")
		if s := profile.ToCollapsed(apiKind, result, top, appOnly); s != "" {
			fmt.Print(s)
			return nil
		}
		return output.NewPrinter("json").Print(result)
	},
}

func parseProfileTimestamp(s string) (int64, error) {
	if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
		return ms, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0, fmt.Errorf("cannot parse time %q: use RFC3339 or epoch millis", s)
	}
	return t.UnixMilli(), nil
}

func init() {
	execProfileCmd.Flags().StringP("kind", "k", "", "analysis kind: hotspots, threads, memory, memory-details (required)")
	execProfileCmd.Flags().StringP("entity", "e", "", "entity ID — PROCESS-xxx, PROCESS_GROUP-xxx, or PROCESS_GROUP_INSTANCE-xxx (services not eligible) (required)")
	execProfileCmd.Flags().String("last", "", "time window relative to now, e.g. 1h, 30m (max 24h)")
	execProfileCmd.Flags().String("from", "", "window start — RFC3339 or epoch millis")
	execProfileCmd.Flags().String("to", "", "window end — RFC3339 or epoch millis")

	// hotspots / threads only
	execProfileCmd.Flags().String("leaf-type", "total", "CPU sample scope (hotspots/threads): total, service, or background")
	execProfileCmd.Flags().Bool("show-waiting", false, "include waiting (non-running) samples (hotspots/threads)")

	// memory* only
	execProfileCmd.Flags().String("problem-id", "", "scope to a problem ID (memory*)")
	execProfileCmd.Flags().Bool("survivors-only", false, "restrict to surviving allocations (memory*)")
	execProfileCmd.Flags().String("type-filter", "", "`;`-separated allocated-type filter (memory*)")
	execProfileCmd.Flags().String("api-filter", "", "`;`-separated API filter (memory*)")
	execProfileCmd.Flags().String("method-fqn-filter", "", "`;`-separated method FQN filter (memory*)")

	// memory-details only
	execProfileCmd.Flags().String("type", "", "allocated type to drill into (memory-details)")
	execProfileCmd.Flags().String("method", "", "method to drill into (memory-details)")

	execProfileCmd.Flags().Int("top", 0, "limit output to top N rows by running samples (0 = all)")
	execProfileCmd.Flags().StringP("output", "o", "", "output format: json")
	execProfileCmd.Flags().Bool("app-only", false, "show only application frames (profiler-classified app code), stripping JRE/library and OneAgent noise")

	_ = execProfileCmd.MarkFlagRequired("kind")
	_ = execProfileCmd.MarkFlagRequired("entity")
}

// startSpinner writes an animated spinner to stderr while work is in progress.
// Call the returned function to stop it and clear the line.
// Skipped in plain/agent mode (non-TTY) to keep output machine-readable.
func startSpinner(label string) func() {
	if agentMode || plainMode || !output.ColorEnabled() {
		return func() {}
	}
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		i := 0
		for {
			select {
			case <-done:
				fmt.Fprintf(os.Stderr, "\r\033[K") // clear line
				return
			case <-ticker.C:
				fmt.Fprintf(os.Stderr, "\r%s %s", frames[i%len(frames)], label)
				i++
			}
		}
	}()
	return func() { close(done) }
}

// visualOnlyKeys are fields that carry UI rendering metadata with no analytical value.
var visualOnlyKeys = map[string]bool{
	"color":    true, // hex colour for charts
	"uuid":     true, // internal render ID
	"fileName": true, // usually null in profiling results
	"filePath": true, // usually null in profiling results
}

// compactResult strips timeseries dataPoints and visual-only fields so agents
// receive signal without chart/render noise.
func compactResult(v interface{}) interface{} {
	switch val := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(val))
		for k, child := range val {
			if visualOnlyKeys[k] {
				continue
			}
			if k == "dataPoints" {
				if arr, ok := child.([]interface{}); ok {
					out["dataPointsCount"] = len(arr)
					continue
				}
			}
			out[k] = compactResult(child)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(val))
		for i, item := range val {
			out[i] = compactResult(item)
		}
		return out
	}
	return v
}
