# Benchmarks

These numbers measure correctness, not speed. The question is whether undo ever leaves the database wrong without saying so.

## Randomized differential test

[e2e/random_test.go](../e2e/random_test.go) generates random agent runs with people's edits mixed in, undoes each run, and compares the result with a control database that never saw the agent.

### Result

4,000 runs, 1,000 on each of PostgreSQL 15, 16, 17 and 18, measured on November 12, 2026:

| | PG 15.19 | PG 16.15 | PG 17.11 | PG 18.6 | Total |
| --- | ---: | ---: | ---: | ---: | ---: |
| Seeds | 1–1000 | 1001–2000 | 2001–3000 | 3001–4000 | 4,000 runs |
| Agent transactions | 5,080 | 5,081 | 4,970 | 4,935 | 20,066 |
| Agent row changes recorded | 13,433 | 13,457 | 13,317 | 12,984 | 53,191 |
| Edits by people | 12,241 | 12,042 | 11,854 | 12,016 | 48,153 |
| Rows restored | 8,658 | 8,702 | 8,728 | 8,449 | 34,537 |
| Rows with a reported conflict (keep or partial) | 2,296 | 2,326 | 2,229 | 2,289 | 9,140 |
| Rows already as before the run (nothing) | 311 | 270 | 298 | 291 | 1,170 |
| Rows refused | 0 | 0 | 0 | 0 | 0 |
| Runs that match the control exactly | 810 | 786 | 770 | 753 | 3,119 |
| Rows that differ from the control, all reported | 210 | 256 | 252 | 281 | 999 |
| **Silent errors** | **0** | **0** | **0** | **0** | **0** |
| Conflicts on rows no person wrote | 0 | 0 | 0 | 0 | 0 |

29 of the 20,066 agent transactions failed and rolled back (for example, an order for a customer the same transaction had just deleted). Those transactions changed nothing, so undo has nothing to do for them.

The 999 rows that differ from the control are rows undo kept on purpose and reported. For example, if a person edited a row the agent had inserted, undo keeps the row and reports a conflict, but the control never had that row.

### How a run works

1. Build two tables with random rows: `customers` and `orders`, joined by a foreign key with `ON DELETE CASCADE`. The tables use text, integer, numeric and JSON values, NULLs, and large text stored out of line.
2. Take 6 to 16 steps. Each step is either an agent transaction of 1 to 3 random statements (insert, update, delete, multi-row updates, cascading deletes), or 1 to 3 edits by people. Most edits by people target rows the agent changed, so conflicts are common.
3. The agent's transactions run only in the recorded database. People's edits run in both databases.
4. Wait for the recorder to catch up, preview the undo, and apply the previewed plan.
5. Compare the two databases row by row and column by column.

A row that differs from the control must be explained by what undo reported: a conflict on that column, a conflict on the whole row, or a refusal. Anything else is a **silent error**, and the test fails and prints the seed and every statement needed to reproduce it.

People's edits follow two rules, so the control is a fair answer key:

- People write rows that exist in both databases, or rows the agent inserted. They never write a row the agent deleted, because they couldn't see it.
- People never write a value the agent wrote. Undo compares values, so it can't tell the two apart. This is a documented limit (see the README).

"Conflicts on rows no person wrote" counts conflicts that no edit by a person explains. It measures false conflicts, and it was 0.

### Checking that the test can fail

A test that never fails proves little. Two bugs were added to the undo engine on purpose, one at a time, and the test was run on seeds 1–100 (PostgreSQL 17.11):

| Change in `internal/undo/history.go` | Silent errors found |
| --- | ---: |
| None | 0 |
| Bug A: always put the old value back, even over a person's later edit (`case receipt.Same(now, c.New[col]):` changed to `case true:`) | 114 |
| Bug B: never re-insert a row the agent deleted (`tg.exists, tg.row = true, keep(t, c.Old)` changed to `tg.exists = false`) | 325 |

To repeat this, make one of the changes, run the command below with `AGENT_UNDO_RANDOM_RUNS=100`, then restore the file with `git checkout internal/undo/history.go`.

### Reproduce

Every choice in a run comes from its seed, so the same seeds should give the same counts. Two separate runs of seeds 1–100 gave identical counts.

```bash
# One PostgreSQL version at a time. Seeds used: 15 → 1, 16 → 1001, 17 → 2001, 18 → 3001.
PG_VERSION=15 docker compose up -d --wait
AGENT_UNDO_RANDOM_RUNS=1000 AGENT_UNDO_RANDOM_SEED=1 \
  go test ./e2e -run '^TestRandomRuns$' -count=1 -timeout 2h -parallel 6 -v | grep -A7 'random runs:'
docker compose down -v   # deletes the container's data
```

For the numbers above, the four versions ran at the same time, each in its own container with the server settings from `docker-compose.yml`, on ports 55415 to 55418 through `AGENT_UNDO_TEST_DB`. Each version took 13.5 to 13.8 minutes.

CI runs the same test with its defaults (seeds 1–20) and the race detector on every push, on all four versions.

### Machine

- Apple M4 Pro (14 cores), 24 GB memory, macOS 26.6.2
- Docker 29.0.1, with the Docker VM limited to 8 CPUs and 7.75 GiB memory
- Go 1.27.4
- PostgreSQL images `postgres:15`, `postgres:16`, `postgres:17` and `postgres:18` (15.19, 16.15, 17.11 and 18.6)

### Limits

- Steps run one after another. People's edits fall between the agent's transactions, not inside them. Transactions that are open while undo runs are covered by `TestSealWaitsForOpenTransactions`, `TestSealStopsTransactionsThatBeganBefore` and `TestApplyNeedsTheSamePlan`.
- The random runs use one schema shape, so they never hit a refusal. Primary key changes, tables without a primary key, user triggers, `TRUNCATE`, partitions and column changes are covered by the 16 scenarios in [e2e/hardcases_test.go](../e2e/hardcases_test.go).
- The numbers say nothing about speed. The run times above are only there to help you plan a rerun.

## Crash test

[e2e/crash_test.go](../e2e/crash_test.go) has an agent commit 600 single-row transactions while the recorder, a separate process, is killed with SIGKILL every 50 to 300 ms. Every row must be in the receipt exactly once. In the run on November 12, 2026 (PostgreSQL 17.11, the machine above), the recorder was killed 18 times, and all 600 rows were recorded exactly once. The number of kills varies from run to run.

```bash
go test ./e2e -run '^TestCrashedRecorderLosesNothingAndRepeatsNothing$' -count=1 -v | grep 'killed the recorder'
```
