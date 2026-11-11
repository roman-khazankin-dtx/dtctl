# Code-Level Profiling (`dtctl exec profiling`)

> **Experimental.** Both the underlying code-level-analysis API and these commands
> may change in breaking ways between releases. Do not build automations directly
> against the underlying endpoint — drive it through this command, which will be
> re-pointed if the API changes.

`dtctl exec profiling` runs a code-level profiling analysis on a monitored process
and returns **collapsed (folded) stacks** — the representation an agent (or a human)
can read directly, rather than the API's UI render tree.

```
dtctl exec profiling hotspots <entity> [flags]
dtctl exec profiling memory   <entity> [flags]
```

## Entities

Only **process-level** entities are accepted — `PROCESS_GROUP` and
`PROCESS_GROUP_INSTANCE`. `SERVICE` entities are **not** eligible.

A modern `PROCESS-xxx` id is accepted and rewritten to its
`PROCESS_GROUP_INSTANCE-xxx` form (same entity, same id suffix; the API 404s on the
`PROCESS-` prefix). The rewrite is intentional — this command speaks the classic
entity vocabulary because the API does.

Find eligible ids from a modern `PROCESS` node:

```
dtctl query 'smartscapeNodes PROCESS
  | fields id, id_classic, dt.process_group.id, dt.process_group.detected_name'
```

- `id_classic` is the `PROCESS_GROUP_INSTANCE-xxx` id.
- `dt.process_group.id` is the `PROCESS_GROUP-xxx` id.

## Output

Folded stacks — one line per unique stack, sorted by RUNNING, carrying all five
thread-state counts so a single read shows whether a stack is CPU-bound or blocked:

```
com.example.OrderController.list;com.example.OrdersOverview.build  running=412 lock=0 net_io=0 disk_io=0 wait=3
com.example.ManufacturerController.issueCreditCard;...reserveSlot  running=2   lock=388 net_io=0 disk_io=0 wait=0
```

- Each frame's own (**self**) samples are attributed to its stack (`self = node −
  Σ children`, since the API's counts are cumulative), so the counts reconcile with
  the analysis totals — not just the leaves.
- This is deliberately **not** single-metric folded, so it does not feed
  `flamegraph.pl`/`speedscope` directly; the multi-counter line keeps lock/IO/wait
  visible, which is what makes off-CPU and lock-contention cases diagnosable.
- `--limit N` caps the number of rows (default 100; `0` = all); it never changes the
  shape.
- Pass `-o json` for the raw compacted result; `--jq` and `-A` (agent envelope)
  compose as usual.
- An empty result prints one `# no stacks for …` line saying why: no samples in
  the window, or `--app-only` removed every frame.

## Flags

| Flag | Kinds | Meaning |
|------|-------|---------|
| `--default-timeframe-start` / `--default-timeframe-end` | all | Window — ISO-8601/RFC3339, epoch millis, or `now()`-relative (e.g. `now()-6h`). Defaults to the last 2h. The window is at most 2h long but can lie anywhere in the past; a start in the future is rejected. |
| `--app-only` | all | Show only application frames (profiler `apiInfo.systemApi`), stripping JRE/library/agent noise. *Note: the profiler's classification is not uniform across languages — on .NET some OneAgent frames are flagged as application code.* |
| `--limit N` | all | Cap output to the top N stacks by running samples (`0` = all). |
| `--leaf-type total\|service\|background` | hotspots | Which CPU samples to attribute. `service` = inside a traced request context (reach for this to localize an endpoint); `background` = ambient (GC, JIT, pools); `total` = everything (default). |
| `--show-waiting` | hotspots | Include waiting (non-running) samples. |
| `--survivors-only` | memory | Restrict to allocations that survived at least one GC in the window. |

## Examples

```bash
# CPU hotspots for a process, application frames only, last hour
dtctl exec profiling hotspots PROCESS-ABC123 --app-only --default-timeframe-start 'now()-1h'

# An earlier 2h window (yesterday's incident)
dtctl exec profiling hotspots PROCESS-ABC123 \
  --default-timeframe-start 'now()-21h' --default-timeframe-end 'now()-19h'

# Localize why a specific endpoint is hot
dtctl exec profiling hotspots PROCESS_GROUP-ABC123 --leaf-type service --limit 5

# Memory allocation pressure, only survivors (the leak tell)
dtctl exec profiling memory PROCESS_GROUP-DEF456 --survivors-only

# Raw result for scripting
dtctl exec profiling hotspots PROCESS-ABC123 -o json --jq '.result.dataNodes | length'
```

## Auth

Read-only. Requires `storage:entities:read` + `storage:profiles:read`, both on the
token and in the IAM policy. Both are part of dtctl's OAuth scope set, so after
`dtctl auth login` nothing else is needed; a platform token needs both scopes added.

## Rate limiting

An analysis is expensive server-side, so an environment limits how many run at
once and answers the rest with `429 Too Many Requests`. dtctl waits and submits
again (honoring `Retry-After` when the server sends one) a few times before it
gives up with a 429 error. Avoid running many analyses in parallel.
