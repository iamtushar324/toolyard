# Code mode limits

Defaults. An administrator can change the concurrency settings; the
`executeToolCode` description states the per-run limits your gateway
enforces.

## Per run

| Limit | Value | What happens past it |
|---|---|---|
| Wall clock | 5 minutes | The run ends, kind `timeout`. A call it cut short makes the run `outcome_unknown`. |
| Tool calls | 100 | The call is refused and the run ends, kind `limit`. Split the work across runs. |
| Output (prints and response text) | 1 MiB | Tool echoes may use three quarters of it; past that the echo stops, your prints and result still fit. |
| Returned value | 1 MiB as JSON | The run fails, kind `limit`. Return less, or print a summary. |
| Starlark execution steps | 50,000,000 | The run ends, kind `limit`. |
| Worker memory | 256 MiB | A JavaScript heap is 88 MiB of it; past it is a catchable `out of memory` error. |
| Calls in flight | 8 | More wait in order. `parallel` defaults to 4. |
| Script size (REST) | 1 MiB | 413. |

## Per call

| Limit | Value |
|---|---|
| Arguments | 4 MiB as JSON |
| One tool result | 6 MiB (a larger one is a failed call, kind `limit`, but the tool already ran: check the target before any retry, never repeat a write blindly) |
| One print line | 1 MiB |
| A failed call's error text | 64 KiB |

## Concurrency

| Limit | Default |
|---|---|
| Scripts running at once on one gateway | 4 |
| Scripts running at once for one agent | 3 |
| Wait for a free slot | 30 seconds, in one fair queue |
| Queue length | twice the per-agent cap for one agent; four times the global cap (16 at least) in all |

A program past a cap waits for a slot. After the wait it is refused as busy:
the message starts `code mode is busy:` and says which cap was full (all
scripts running, or your own per-agent cap). Over REST that run ends `failed` with kind `busy`, HTTP 429 with
`Retry-After`, and its `Idempotency-Key` is free again. A program past the
queue length is refused at once. A freed slot goes to the earliest waiter
whose agent is under its own cap, so one agent's backlog never holds another
agent up.

## Interruptions

A run that toolyard stops because the server shuts down, or because the
client went away, is kind `interrupted`: `failed` (nothing ran) when no tool
call had been sent, `outcome_unknown` once one had. A REST run still
`queued` when the process dies is `failed` after the restart, and one still
`running` is `outcome_unknown`, both with kind `interrupted`.

## tools.search

| Argument | Values |
|---|---|
| `query` | Words (keyword) or a pattern (regex). Empty lists every tool by name. At most 1,000 characters; only the first 32 distinct words count. |
| `mode` | `keyword` (default, ranked), `regex` (RE2, at most 200 characters, case-insensitive unless `(?-i)`), `auto` (keyword). `semantic` is an error unless the operator configured it. |
| `detail` | `name`, `summary`, `full` (adds input and output schemas). Default `full` for a direct MCP call, `summary` inside a script and over REST. |
| `limit` | 50 by default, 200 at most. |
| `upstream` | Only one server's tools. |

A regex search stops after 250 ms; one agent runs at most 2 at once, and a
third waits up to 2 seconds, then is refused. A too-complex pattern is
refused before it runs: simplify it, or use `keyword`.

## Retention

REST runs and their call records are kept 7 days from their start.
