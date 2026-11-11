package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Dynatrace/dtctl/pkg/diagnostic"
	"github.com/Dynatrace/dtctl/pkg/output"
	"github.com/Dynatrace/dtctl/pkg/resources/profile"
	"github.com/Dynatrace/dtctl/pkg/safety"
	"github.com/Dynatrace/dtctl/pkg/stability"
	"github.com/Dynatrace/dtctl/sdk/httpclient"
)

// profilingSince is the release in which `exec profiling` entered the
// experimental tier. One constant for the whole family.
const profilingSince = "0.46.0"

const (
	// profilingDefaultWindowMs is the window used when no start is given.
	profilingDefaultWindowMs = 2 * 60 * 60 * 1000
	// profilingMaxWindowMs caps the window length. The API itself accepts up to
	// 24h, but a wider window costs the server proportionally more, and 2h is
	// enough to localize a hotspot; how far back the window lies is not limited.
	profilingMaxWindowMs = 2 * 60 * 60 * 1000
)

// profilingFindIDsQuery lists eligible entity ids.
const profilingFindIDsQuery = `dtctl query 'smartscapeNodes PROCESS | fields id, id_classic, dt.process_group.id, dt.process_group.detected_name'`

// profilingFindIDsHint tells the caller how to find an eligible entity id.
const profilingFindIDsHint = profilingFindIDsQuery + `
  # id_classic is the PROCESS_GROUP_INSTANCE-xxx id; dt.process_group.id is the PROCESS_GROUP-xxx id`

// execProfilingCmd is the namespace for code-level profiling analyses.
var execProfilingCmd = newExecProfilingCmd()

// newExecProfilingCmd builds `exec profiling` with its kind subcommands. The
// kind is a subcommand (hotspots/memory) so each declares only the flags it
// accepts and cobra rejects nonsensical combinations; the entity is a
// positional argument.
func newExecProfilingCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "profiling",
		Short: "Run a code-level profiling analysis on a process",
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
changes the shape. Pass -o json for the compacted structured result.

Kinds:
  hotspots  Method CPU hotspots (per-stack thread-state counts include lock/IO/wait)
  memory    Memory allocation analysis (allocation pressure, not live heap)

Entities: only PROCESS_GROUP and PROCESS_GROUP_INSTANCE are accepted — the API
does not accept SERVICE. This command speaks that classic entity vocabulary
because the API does; it will change when the API's successor lands. A modern
PROCESS-xxx id is accepted and rewritten to its PROCESS_GROUP_INSTANCE-xxx form
(same entity, same id suffix).

Find eligible ids:
  ` + profilingFindIDsHint + `

Timeframe: --default-timeframe-start/--default-timeframe-end (ISO-8601/RFC3339,
epoch millis, or now()-relative). Defaults to the last 2h. The window must not
exceed 2h, but it can lie anywhere in the past.

Required token scopes: storage:entities:read, storage:profiles:read.`,
		RunE: requireSubcommand,
	}
	stability.Mark(c, stability.Experimental, profilingSince)
	c.AddCommand(newProfilingHotspotsCmd(), newProfilingMemoryCmd())
	return c
}

const profilingLeafTypeHelp = `
--leaf-type picks which CPU samples to attribute:
  total       (default) everything, including agent/framework overhead
  service     samples taken inside a traced service/request context — reach for
              this to localize why a specific endpoint or call path is hot
  background  ambient work outside any request (GC, JIT, schedulers, pools)`

// profilingFlags are one command's flag values. Flags a kind does not register
// stay at their zero value, so one run body serves every kind.
type profilingFlags struct {
	start, end    string
	limit         int
	appOnly       bool
	leafType      string
	showWaiting   bool
	survivorsOnly bool
}

func newProfilingHotspotsCmd() *cobra.Command {
	f := &profilingFlags{}
	c := &cobra.Command{
		Use:   "hotspots <entity>",
		Short: "Method CPU hotspots for a process group or instance",
		Long: `Method CPU hotspots for a PROCESS_GROUP or PROCESS_GROUP_INSTANCE.
` + profilingLeafTypeHelp + `

Examples:
  dtctl exec profiling hotspots PROCESS-ABC123          # rewritten to PGI
  dtctl exec profiling hotspots PROCESS_GROUP-ABC123 --leaf-type service --limit 5
  dtctl exec profiling hotspots PROCESS_GROUP_INSTANCE-ABC123 --app-only \
    --default-timeframe-start 'now()-1h'
  # an earlier 2h window
  dtctl exec profiling hotspots PROCESS-ABC123 \
    --default-timeframe-start 'now()-21h' --default-timeframe-end 'now()-19h'`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProfiling(cmd, "hotspots", args[0], f)
		},
	}
	addProfilingCommonFlags(c, f)
	c.Flags().StringVar(&f.leafType, "leaf-type", profile.LeafTypeTotal, "CPU sample scope: total, service, or background")
	c.Flags().BoolVar(&f.showWaiting, "show-waiting", false, "include waiting (non-running) samples")
	stability.Mark(c, stability.Experimental, profilingSince)
	return c
}

func newProfilingMemoryCmd() *cobra.Command {
	f := &profilingFlags{}
	c := &cobra.Command{
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
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProfiling(cmd, "memory", args[0], f)
		},
	}
	addProfilingCommonFlags(c, f)
	c.Flags().BoolVar(&f.survivorsOnly, "survivors-only", false, "restrict to surviving allocations")
	stability.Mark(c, stability.Experimental, profilingSince)
	return c
}

func addProfilingCommonFlags(c *cobra.Command, f *profilingFlags) {
	fl := c.Flags()
	fl.StringVar(&f.start, "default-timeframe-start", "", "window start — ISO-8601/RFC3339, epoch millis, or now()-<dur> (default: now()-2h)")
	fl.StringVar(&f.end, "default-timeframe-end", "", "window end — ISO-8601/RFC3339, epoch millis, or now() (default: now(); window max 2h)")
	fl.IntVar(&f.limit, "limit", 100, "cap output to the top N stacks: hotspots by running samples, memory by allocated bytes (0 = all)")
	fl.BoolVar(&f.appOnly, "app-only", false, "show only application frames (profiler-classified app code), stripping JRE/library and OneAgent noise")
}

// runProfiling validates the input, runs one analysis and prints it. Every
// input check happens before the config is loaded, so a typo fails fast
// rather than after a multi-second analysis.
func runProfiling(cmd *cobra.Command, kind, entityID string, f *profilingFlags) error {
	ctx := cmdContext(cmd)

	if !isEligibleProfilingEntity(entityID) {
		return fmt.Errorf("invalid entity %q: only PROCESS, PROCESS_GROUP, and PROCESS_GROUP_INSTANCE are eligible for profiling (SERVICE is not).\nFind an eligible id:\n  %s", entityID, profilingFindIDsHint)
	}
	switch f.leafType {
	case "", profile.LeafTypeTotal, profile.LeafTypeService, profile.LeafTypeBackground:
	default:
		return fmt.Errorf("invalid --leaf-type %q: use total, service, or background", f.leafType)
	}
	from, to, err := resolveProfilingTimeframe(f.start, f.end)
	if err != nil {
		return err
	}

	// Code-level analysis only reads profiling data — gate on OperationRead.
	_, c, err := setupWithSafety(ctx, safety.OperationRead)
	if err != nil {
		return err
	}

	apiKind := profile.Kind[kind]
	stopSpinner := startSpinner(ctx, fmt.Sprintf("running %s analysis", kind))
	resp, err := profile.NewHandler(c).Run(cmd.Context(), profile.Payload{
		Kind:          apiKind,
		EntityID:      entityID,
		From:          from,
		To:            to,
		LeafType:      f.leafType,
		ShowWaiting:   f.showWaiting,
		SurvivorsOnly: f.survivorsOnly,
	})
	stopSpinner()
	if err != nil {
		return profilingAPIError(err)
	}

	// Round-trip through JSON so the renderers and --jq see the plain envelope
	// shape: {"status":..., "kind":..., "entityId":..., "result":{...}}.
	b, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	var result interface{}
	if err := json.Unmarshal(b, &result); err != nil {
		return err
	}
	return renderProfiling(ctx, apiKind, compactResult(result), f.limit, f.appOnly)
}

// profilingAPIError adds profiling-specific advice to the API failures a
// caller can act on. Everything else passes through unchanged and gets dtctl's
// generic handling.
func profilingAPIError(err error) error {
	var apiErr *httpclient.APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	var suggestions []string
	switch apiErr.StatusCode {
	case 400:
		suggestions = []string{
			"The API rejected the request parameters — check the timeframe (in the past, at most 2h) and the entity id",
		}
	case 403:
		suggestions = []string{
			"Profiling needs storage:profiles:read and storage:entities:read, both on the token and in the IAM policy",
			"OAuth (dtctl auth login): log in again so the session requests storage:profiles:read",
			"Re-run the same command with --check-scopes to verify the token's scopes",
		}
	case 404:
		suggestions = []string{
			"The environment knows no such process (ids are per environment, and short-lived process instances disappear)",
			"List eligible ids: " + profilingFindIDsQuery,
		}
	case 429:
		suggestions = []string{
			"The environment limits concurrent profiling analyses and is at that limit — wait and retry, and avoid running many analyses in parallel",
		}
	default:
		return err
	}
	return &diagnostic.Error{
		Operation:   "run profiling analysis",
		StatusCode:  apiErr.StatusCode,
		Message:     apiErr.Message,
		Suggestions: suggestions,
		Err:         err,
	}
}

// renderProfiling routes output through the standard printer so --jq and the
// agent envelope compose. Folded stacks are the default; a structured -o
// (json/yaml/toon/…) or --jq prints the compacted result instead. In agent mode
// the folded text is carried inside the envelope rather than written raw.
func renderProfiling(ctx context.Context, apiKind string, result interface{}, limit int, appOnly bool) error {
	printValue := func(v interface{}) error {
		p := newPrinterCtx(ctx)
		enrichAgent(p, "exec", "profiling")
		return p.Print(v)
	}
	switch outputFormat(ctx) {
	case "json", "yaml", "yml", "toon", "csv", "jsonl", "parquet":
		return printValue(result)
	}
	if jqFilter(ctx) != "" {
		return printValue(result)
	}

	// table (the -o default), wide, folded, or unset -> folded stacks
	s := profile.ToCollapsed(apiKind, result, limit, appOnly)
	if s == "" {
		// No folded output. Say why in one line rather than dumping the raw
		// result (hundreds of lines of render metadata with nothing in it);
		// -o json still prints the full result.
		s = noProfilingStacksNote(apiKind, result, appOnly) + "\n"
	}
	if agentMode(ctx) {
		return printValue(s)
	}
	_, err := fmt.Fprint(currentStdout(ctx), s)
	return err
}

// noProfilingStacksNote explains an empty folded result: either the window
// holds no samples for the entity at all, or --app-only removed every frame.
func noProfilingStacksNote(apiKind string, result interface{}, appOnly bool) string {
	entity, status := "", ""
	if m, ok := result.(map[string]interface{}); ok {
		entity, _ = m["entityId"].(string)
		status, _ = m["status"].(string)
	}
	if appOnly && profile.ToCollapsed(apiKind, result, 1, false) != "" {
		return fmt.Sprintf("# no stacks for %s: --app-only removed every frame (the profiler classified none as application code) — re-run without --app-only", entity)
	}
	return fmt.Sprintf("# no stacks for %s: no profiling samples in the window (status=%s) — the process may be idle, restarted, or not under load; try another window (at most 2h), or -o json for the raw result", entity, status)
}

// resolveProfilingTimeframe turns the --default-timeframe-start/-end values
// into an epoch-millis window. Both accept ISO-8601/RFC3339, epoch millis, or a
// now()-relative expression; an empty end defaults to now and an empty start to
// two hours before the end. The window must start in the past, be positive and
// be at most profilingMaxWindowMs, checked here so an oversized window never
// reaches the API.
func resolveProfilingTimeframe(startStr, endStr string) (int64, int64, error) {
	nowMs := time.Now().UnixMilli()

	to := nowMs
	if endStr != "" {
		var err error
		if to, err = parseProfilingTime(endStr, nowMs); err != nil {
			return 0, 0, fmt.Errorf("invalid --default-timeframe-end: %w", err)
		}
	}

	from := to - profilingDefaultWindowMs
	if startStr != "" {
		var err error
		if from, err = parseProfilingTime(startStr, nowMs); err != nil {
			return 0, 0, fmt.Errorf("invalid --default-timeframe-start: %w", err)
		}
	}

	if from >= nowMs {
		return 0, 0, fmt.Errorf("invalid timeframe: the start is in the future — profiling only covers the past (e.g. --default-timeframe-start 'now()-2h')")
	}
	if to <= from {
		return 0, 0, fmt.Errorf("invalid timeframe: the end must be after the start")
	}
	if to-from > profilingMaxWindowMs {
		return 0, 0, fmt.Errorf("invalid timeframe: the window must not exceed 2h (got %s) — narrow it; the window can lie anywhere in the past", time.Duration(to-from)*time.Millisecond)
	}
	return from, to, nil
}

// parseProfilingTime parses one timeframe bound: a now()-relative expression
// (now(), now()-90min, now()+30min), epoch millis, or RFC3339.
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

// startSpinner writes an animated spinner to stderr while work is in progress.
// Call the returned function to stop it and clear the line.
// Skipped in plain/agent mode (non-TTY) to keep output machine-readable.
func startSpinner(ctx context.Context, label string) func() {
	if agentMode(ctx) || plainMode(ctx) || !output.ColorEnabled() {
		return func() {}
	}
	stderr := currentStderr(ctx)
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		i := 0
		for {
			select {
			case <-done:
				fmt.Fprintf(stderr, "\r\033[K") // clear line
				return
			case <-ticker.C:
				fmt.Fprintf(stderr, "\r%s %s", frames[i%len(frames)], label)
				i++
			}
		}
	}()
	// Wait for the goroutine to clear the line, so nothing it writes can land
	// after the command's own output.
	return func() { close(done); <-stopped }
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
