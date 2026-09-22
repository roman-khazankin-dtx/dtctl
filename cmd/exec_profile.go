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

// execProfilingCmd is the namespace for code-level profiling analyses. The kind
// is a subcommand (hotspots/threads/memory) so each declares only the flags it
// actually accepts and cobra rejects nonsensical combinations; the entity is a
// positional argument.
var execProfilingCmd = &cobra.Command{
	Use:   "profiling",
	Short: "Run a code-level profiling analysis on a process entity (experimental)",
	Long: `Run a code-level profiling analysis via the Dynatrace code-level analysis API.

EXPERIMENTAL: both the underlying API and these commands may change in breaking
ways between releases.

The analysis is asynchronous server-side; the command polls until it completes.
Output is collapsed (folded) stacks — one line per unique stack, carrying all
five thread-state counts so a single read shows whether a stack is CPU-bound or
blocked (this is not single-metric folded, so it does not feed flamegraph tools
directly):
  frame1;frame2;...;frameN running=16 lock=0 net_io=0 disk_io=0 wait=0
Each frame's own (self) samples are attributed to its stack, so the counts
reconcile with the analysis totals. --limit caps the number of rows; it never
changes the shape. Pass -o json for the raw compacted result.

Kinds:
  hotspots  Method CPU hotspots (per-stack thread-state counts include lock/IO/wait)
  memory    Memory allocation analysis (allocation pressure, not live heap)

Entities: only PROCESS_GROUP and PROCESS_GROUP_INSTANCE are accepted — the API
does not accept SERVICE. This command speaks that classic entity vocabulary
because the API does; it will change when the API's successor lands. A modern
PROCESS-xxx id is accepted and rewritten to its PROCESS_GROUP_INSTANCE-xxx form
(same entity, same id suffix — the API 404s on the PROCESS- prefix).

Find eligible ids from a modern PROCESS node:
  dtctl query 'smartscapeNodes PROCESS
    | fields id, id_classic, dt.process_group.id, dt.process_group.detected_name'
  # id_classic is the PROCESS_GROUP_INSTANCE-xxx id; dt.process_group.id is the PROCESS_GROUP-xxx id.

Timeframe: reuses the dtctl-wide --default-timeframe-start/--default-timeframe-end
flags (ISO-8601/RFC3339, epoch millis, or now()-relative). Defaults to the last
2h; the window must not exceed 24h.`,
	RunE: requireSubcommand,
}

const profilingLeafTypeHelp = `
--leaf-type picks which CPU samples to attribute:
  total       (default) everything, including agent/framework overhead
  service     samples taken inside a traced service/request context — reach for
              this to localize why a specific endpoint or call path is hot
  background  ambient work outside any request (GC, JIT, schedulers, pools)`

var profilingHotspotsCmd = &cobra.Command{
	Use:   "hotspots <entity>",
	Short: "Method CPU hotspots for a process group or instance",
	Long: `Method CPU hotspots for a PROCESS_GROUP or PROCESS_GROUP_INSTANCE.
` + profilingLeafTypeHelp + `

Examples:
  dtctl exec profiling hotspots PROCESS-ABC123          # rewritten to PGI
  dtctl exec profiling hotspots PROCESS_GROUP-ABC123 --leaf-type service --limit 5
  dtctl exec profiling hotspots PROCESS_GROUP_INSTANCE-ABC123 --app-only \
    --default-timeframe-start now()-6h`,
	Args: cobra.ExactArgs(1),
	RunE: runProfiling("hotspots"),
}

var profilingMemoryCmd = &cobra.Command{
	Use:   "memory <entity>",
	Short: "Memory allocation analysis for a process group or instance",
	Long: `Memory allocation analysis for a PROCESS_GROUP or PROCESS_GROUP_INSTANCE.

Shows where allocations happen and what survived GC within the window —
allocation pressure, not live retained heap. It does not prove a leak or predict
OOM; confirm that with a heap/RSS trend and pod restart/OOM state. The leak tell
is survived/allocated: a frame whose survivors far exceed its in-window
allocations is retaining, not churning (the biggest allocator is usually churn).
NOTE the memory tree is INVERTED vs hotspots: the allocation-site method is at
the root and its callers hang below as children.

Examples:
  dtctl exec profiling memory PROCESS_GROUP-DEF456
  dtctl exec profiling memory PROCESS_GROUP-DEF456 --survivors-only`,
	Args: cobra.ExactArgs(1),
	RunE: runProfiling("memory"),
}

// runProfiling builds the RunE for one analysis kind. The same body serves every
// kind: flags a kind does not register read back as their zero value, so the
// payload is assembled uniformly.
func runProfiling(kindShort string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		entityID := args[0]
		apiKind := profile.Kind[kindShort]

		if !isEligibleProfilingEntity(entityID) {
			return fmt.Errorf(`entity %q is not eligible for profiling — only PROCESS, PROCESS_GROUP, and PROCESS_GROUP_INSTANCE are accepted (SERVICE is not).
Find an eligible id from a modern PROCESS node:
  dtctl query 'smartscapeNodes PROCESS | fields id, id_classic, dt.process_group.id, dt.process_group.detected_name'
  # id_classic is the PROCESS_GROUP_INSTANCE-xxx id; dt.process_group.id is the PROCESS_GROUP-xxx id`, entityID)
		}

		startStr, _ := cmd.Flags().GetString("default-timeframe-start")
		endStr, _ := cmd.Flags().GetString("default-timeframe-end")
		from, to, err := resolveProfilingTimeframe(startStr, endStr)
		if err != nil {
			return err
		}

		leafType, _ := cmd.Flags().GetString("leaf-type")
		showWaiting, _ := cmd.Flags().GetBool("show-waiting")
		survivorsOnly, _ := cmd.Flags().GetBool("survivors-only")
		appOnly, _ := cmd.Flags().GetBool("app-only")
		limit, _ := cmd.Flags().GetInt("limit")

		// Code-level analysis only reads profiling data — gate on OperationRead.
		_, c, err := SetupWithSafety(safety.OperationRead)
		if err != nil {
			return err
		}

		handler := profile.NewHandler(c)
		stopSpinner := startSpinner(fmt.Sprintf("running %s analysis", kindShort))
		resp, err := handler.Run(cmd.Context(), profile.Payload{
			Kind:          apiKind,
			EntityID:      entityID,
			From:          from,
			To:            to,
			LeafType:      leafType,
			ShowWaiting:   showWaiting,
			SurvivorsOnly: survivorsOnly,
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

		return renderProfiling(apiKind, result, limit, appOnly)
	}
}

// renderProfiling routes output through the standard printer so --jq and the
// agent envelope compose. Folded stacks are the default; a structured -o
// (json/yaml/toon/…) prints the compacted result instead. In agent mode the
// folded text is carried inside the envelope rather than written raw to stdout.
func renderProfiling(apiKind string, result interface{}, limit int, appOnly bool) error {
	switch outputFormat {
	case "json", "yaml", "yml", "toon", "csv", "jsonl", "parquet":
		p := NewPrinter()
		enrichAgent(p, "exec", "profiling")
		return p.Print(result)
	default: // table (the -o default), wide, folded, or unset -> folded stacks
		s := profile.ToCollapsed(apiKind, result, limit, appOnly)
		if s == "" {
			// No folded output — no samples in the window, or --app-only stripped
			// every frame. Fall back to the structured result so the caller still
			// sees the status rather than an empty stream.
			p := NewPrinter()
			enrichAgent(p, "exec", "profiling")
			return p.Print(result)
		}
		if agentMode {
			p := NewPrinter()
			enrichAgent(p, "exec", "profiling")
			return p.Print(s)
		}
		fmt.Print(s)
		return nil
	}
}

// resolveProfilingTimeframe turns the --default-timeframe-start/-end flags into
// an epoch-millis window. Both accept ISO-8601/RFC3339, epoch millis, or a
// now()-relative expression; an empty end defaults to now and an empty start to
// two hours before the end. The window must be positive and at most 24h (the
// API's cap, also enforced server-side).
func resolveProfilingTimeframe(startStr, endStr string) (int64, int64, error) {
	nowMs := time.Now().UnixMilli()

	to := nowMs
	if endStr != "" {
		var err error
		if to, err = parseProfilingTime(endStr, nowMs); err != nil {
			return 0, 0, fmt.Errorf("--default-timeframe-end: %w", err)
		}
	}

	from := to - 2*60*60*1000 // default: last 2h
	if startStr != "" {
		var err error
		if from, err = parseProfilingTime(startStr, nowMs); err != nil {
			return 0, 0, fmt.Errorf("--default-timeframe-start: %w", err)
		}
	}

	if to <= from {
		return 0, 0, fmt.Errorf("timeframe end must be after start")
	}
	if to-from > 24*60*60*1000 {
		return 0, 0, fmt.Errorf("timeframe must not exceed 24h")
	}
	return from, to, nil
}

// parseProfilingTime parses one timeframe bound: a now()-relative expression
// (now(), now()-6h, now()+30min), epoch millis, or RFC3339.
func parseProfilingTime(s string, nowMs int64) (int64, error) {
	s = strings.TrimSpace(s)
	if rest, ok := strings.CutPrefix(s, "now()"); ok {
		rest = strings.TrimSpace(rest)
		if rest == "" {
			return nowMs, nil
		}
		var sign int64
		switch rest[0] {
		case '-':
			sign = -1
		case '+':
			sign = 1
		default:
			return 0, fmt.Errorf("expected now()-<duration> or now()+<duration>, got %q", s)
		}
		durStr := strings.ReplaceAll(strings.TrimSpace(rest[1:]), "min", "m")
		d, err := time.ParseDuration(durStr)
		if err != nil {
			return 0, fmt.Errorf("invalid duration in %q: %w", s, err)
		}
		return nowMs + sign*d.Milliseconds(), nil
	}
	return parseProfileTimestamp(s)
}

// isEligibleProfilingEntity reports whether id is a process-level entity the
// code-level analysis API accepts (PROCESS_GROUP, PROCESS_GROUP_INSTANCE, or the
// 3rd-gen PROCESS rename). "PROCESS-" does not prefix-match "PROCESS_GROUP-" (the
// 8th byte is '-' vs '_'), so each type is matched distinctly. The SDK enforces
// the same allowlist; this CLI check exists to fail fast with a next step.
func isEligibleProfilingEntity(id string) bool {
	return strings.HasPrefix(id, "PROCESS-") ||
		strings.HasPrefix(id, "PROCESS_GROUP-") ||
		strings.HasPrefix(id, "PROCESS_GROUP_INSTANCE-")
}

func parseProfileTimestamp(s string) (int64, error) {
	if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
		return ms, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0, fmt.Errorf("cannot parse time %q: use RFC3339, epoch millis, or now()-<dur>", s)
	}
	return t.UnixMilli(), nil
}

func init() {
	execProfilingCmd.AddCommand(profilingHotspotsCmd, profilingMemoryCmd)

	for _, c := range []*cobra.Command{profilingHotspotsCmd, profilingMemoryCmd} {
		addProfilingCommonFlags(c)
	}
	// hotspots
	profilingHotspotsCmd.Flags().String("leaf-type", "total", "CPU sample scope: total, service, or background")
	profilingHotspotsCmd.Flags().Bool("show-waiting", false, "include waiting (non-running) samples")
	// memory
	profilingMemoryCmd.Flags().Bool("survivors-only", false, "restrict to surviving allocations")
}

func addProfilingCommonFlags(c *cobra.Command) {
	c.Flags().String("default-timeframe-start", "", "window start — ISO-8601/RFC3339, epoch millis, or now()-<dur> (default: now()-2h)")
	c.Flags().String("default-timeframe-end", "", "window end — ISO-8601/RFC3339, epoch millis, or now() (default: now(); window max 24h)")
	c.Flags().Int("limit", 100, "cap output to the top N stacks by running samples (0 = all)")
	c.Flags().Bool("app-only", false, "show only application frames (profiler-classified app code), stripping JRE/library and OneAgent noise")
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
