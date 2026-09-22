# Demo: agent diagnoses pizza service slowness

## Prompt to give the fresh agent

> My pizza service is slow. The process group instance is
> `PROCESS_GROUP_INSTANCE-82CBCAA0F214B356` on environment `hzi4275d`.
> I have `dtctl` available. Figure out why.

## What the agent should do (without any hints)

1. Run `dtctl exec --help` or `dtctl commands --brief -o json` to discover available commands
2. Notice `exec profiling` and read its help
3. Run:
   ```
   dtctl exec profiling hotspots \
     PROCESS_GROUP_INSTANCE-82CBCAA0F214B356 \
     --default-timeframe-start now()-1h \
     --app-only
   ```
4. Read the folded stacks (top rows by `running=`) and identify the hot method/path
