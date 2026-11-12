# agent-undo design

November 2026

agent-undo reverses the writes from one AI agent run on a live Postgres database and leaves everyone else's later edits alone.

## Problem

Standard recovery restores the whole database to an earlier moment, so taking back one bad run usually means rewinding everyone else's work too.

Say an agent is supposed to close expired trial accounts, but a bug makes it cancel 333 paying customers and delete 500 of their orders. Meanwhile the support team is still editing customer records. Restoring a backup brings the 833 rows back and throws away every support edit made since. Fixing it by hand means finding all 833 rows and working out what each one looked like before. Google's [Spanner docs](https://docs.cloud.google.com/spanner/docs/pitr) describe partial recovery as the same manual job: read the old data, then write it back.

agent-undo reverses the agent's 833 changes and leaves the support edits alone. The same approach works for bad scripts and human mistakes, as long as their writes are tagged.

This matters more now that agents write to live databases directly. With Google's [MCP Toolbox](https://github.com/googleapis/mcp-toolbox), each agent SQL call runs on its own connection and commits when it finishes, so an agent can't hold its changes open and cancel them later.

## How it works

The agent logs in as its own database user and sets `application_name` to its run ID. Before each statement the agent runs, a trigger writes a tag row naming the run, so in the write-ahead log (WAL) the tag lands just before that statement's changes, inside the agent's transaction. Every table has this trigger, so no write path can skip it, and only the trigger can write tag rows, so tags can't be forged. The trigger also blocks agent writes when recording isn't working. A small Go service reads the WAL through logical decoding and adds every change that follows a tag to that run's receipt. It acknowledges each transaction's end LSN to the replication slot only after the receipt lines are fsynced. The receipt format is in [receipt-format.md](receipt-format.md).

To undo a run, the tool walks the receipt from newest change to oldest and compares each change with the row as it is now, one column at a time. Each comparison ends in one of three results:

| Result | When | What undo does |
| --- | --- | --- |
| Restore | Nobody changed it since, or only other columns changed | Puts the old value back |
| Keep | Someone changed the same column later | Leaves their value and reports a conflict |
| Refuse | There's no safe way back, for example a table with no primary key | Changes nothing |

A later edit to a column the agent changed is always kept and reported as a conflict, and the plan then requires manual review.

In this example the agent cancels a plan, and support later updates the phone number on the same row. Undo puts the plan back and keeps the new number.

| Step | plan | phone |
| --- | --- | --- |
| Before the agent | pro | 555-0100 |
| Agent cancels the plan | cancelled | 555-0100 |
| Support updates the phone | cancelled | 555-0199 |
| After undo | pro | 555-0199 |

If support had changed the plan too, undo would keep support's value and report a conflict.

## Interface

People use a command line. In v1, only a person can apply an undo.

| Command | What it does |
| --- | --- |
| `undo show run-42` | Lists every row run 42 changed, before and after |
| `undo preview run-42` | Shows what undo would restore, keep or refuse, without changing anything, and prints a hash of the plan |
| `undo apply --plan <hash> run-42` | Applies exactly the previewed plan and lists the conflicts that require manual review |

Apply first seals the run, so the database refuses any more writes from it. It then waits until the recorder has caught up and redoes the plan with the rows locked. If the result differs from the previewed plan, it stops.

**Planned, not built yet:** MCP tools, so agents can inspect their own runs:

- `show_changes` would report what the agent's run changed.
- `preview_undo` would report what undo would do.
- `request_undo` would ask a person to approve an undo.

An agent would only see its own runs or ask to undo them. Ordinary code decides what gets restored, not the model, and the server would check approval, so an agent couldn't skip it.

## Related work

The two closest projects are pgbrake and EterDB. By default, both refuse when someone else has since edited the same row. agent-undo keeps that person's edit and undoes the rest.

| | [pgbrake](https://github.com/lexang/pgbrake) | [EterDB](https://github.com/eterdb/eterdb) | agent-undo |
| --- | --- | --- | --- |
| Setup | Agents must send every write through its server | Your app runs on its modified Postgres 18 | Stock Postgres with logical decoding on, a trigger per table and a small service |
| Undoes | One write at a time | One transaction, or a group of them | One whole agent run |
| Someone edited the same row later | Refuses, unless forced to overwrite | Refuses by default; can also undo their edit, or overwrite it | Keeps their edit, restores the rest, reports real conflicts |
| Runs on Cloud SQL or AlloyDB | Yes | No, it needs its own engine | Should work, since both offer logical replication ([Cloud SQL](https://docs.cloud.google.com/sql/docs/postgres/replication/configure-logical-replication), [AlloyDB](https://docs.cloud.google.com/datastream/docs/configure-your-source-postgresql-database)). Not tested yet |

EterDB is ahead in one area. It records reads, so it can list later writes that were based on bad data. That depends on its modified engine. The replay step planned for later closes part of that gap.

## Sources

Pages checked on Nov 6, 2026:

- [pgbrake on GitHub](https://github.com/lexang/pgbrake)
- [EterDB on GitHub](https://github.com/eterdb/eterdb)
- [Spanner point-in-time recovery](https://docs.cloud.google.com/spanner/docs/pitr)
- [Cloud SQL: configure logical replication and decoding](https://docs.cloud.google.com/sql/docs/postgres/replication/configure-logical-replication)
- [Datastream: configure a PostgreSQL source, including AlloyDB](https://docs.cloud.google.com/datastream/docs/configure-your-source-postgresql-database)
- [MCP Toolbox for Databases](https://github.com/googleapis/mcp-toolbox)
- [Postgres: logical decoding concepts](https://www.postgresql.org/docs/current/logicaldecoding-explanation.html)
- [Gunnar Morling: The Insatiable Postgres Replication Slot](https://morling.dev/blog/insatiable-postgres-replication-slot)
