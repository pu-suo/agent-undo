# agent-undo design

November 2026

agent-undo reverses the writes from one AI agent run on a live Postgres database and leaves everyone else's later edits alone.

## Problem

Standard recovery restores the whole database to an earlier moment, so taking back one bad run usually means rewinding everyone else's work too.

Say an agent is supposed to close expired trial accounts, but a bug makes it cancel 333 paying customers and delete 500 of their orders. Meanwhile the support team is still editing customer records. Restoring a backup brings the 833 rows back and throws away every support edit made since. Fixing it by hand means finding all 833 rows and working out what each one looked like before. Google's [Spanner docs](https://docs.cloud.google.com/spanner/docs/pitr) describe partial recovery as the same manual job: read the old data, then write it back.

agent-undo reverses the agent's 833 changes and leaves the support edits alone. The same approach works for bad scripts and human mistakes, as long as their writes are tagged.

This matters more now that agents write to live databases directly. With Google's [MCP Toolbox](https://github.com/googleapis/mcp-toolbox), each agent SQL call runs on its own connection and commits when it finishes, so an agent can't hold its changes open and cancel them later.

## How it works

The agent logs in as its own database user and sets `application_name` to its run ID. After each statement the agent runs, a trigger writes that ID into the WAL (Postgres's write-ahead log) inside the agent's transaction. A small Go service reads the WAL through logical decoding and saves the changes from tagged transactions as a receipt for that run.

To undo a run, the tool walks the receipt from newest change to oldest and compares each change with the row as it is now, one column at a time. Each comparison ends in one of three results:

| Result | When | What undo does |
| --- | --- | --- |
| Restore | Nobody changed it since, or only other columns changed | Puts the old value back |
| Flag | Someone changed the same value later | Leaves it and asks a person |
| Refuse | There's no safe way back, for example a table with no primary key | Changes nothing |

Undo must never silently overwrite a later edit, which is why a flagged change stops and waits for a person.

In this example the agent cancels a plan, and support later updates the phone number on the same row. Undo puts the plan back and keeps the new number.

| Step | plan | phone |
| --- | --- | --- |
| Before the agent | pro | 555-0100 |
| Agent cancels the plan | cancelled | 555-0100 |
| Support updates the phone | cancelled | 555-0199 |
| After undo | pro | 555-0199 |

If support had changed the plan too, undo would leave it alone and flag it for a person.

## Interface

People use a command line. In v1, only a person can apply an undo.

| Command | What it does |
| --- | --- |
| `undo show run-42` | Lists every row run 42 changed, before and after |
| `undo preview run-42` | Shows what undo would restore, flag or refuse, without changing anything |
| `undo apply run-42` | Applies the restores and lists the flagged changes for a person to resolve |

Agents get MCP tools, MCP being the standard way AI agents call outside functions:

- `show_changes` reports what the agent's run changed.
- `preview_undo` reports what undo would do.
- `request_undo` (later) asks a person to approve an undo.

An agent can only look at its own runs or ask to undo them. Ordinary code decides what gets restored, not the model. The server checks approval, so an agent can't skip that step.

## Related work

The two closest projects are pgbrake and EterDB. By default, both refuse when someone else has since edited the same row. agent-undo keeps that person's edit and undoes the rest.

| | [pgbrake](https://github.com/lexang/pgbrake) | [EterDB](https://github.com/eterdb/eterdb) | agent-undo |
| --- | --- | --- | --- |
| Setup | Agents must send every write through its server | Your app runs on its modified Postgres 18 | Stock Postgres with logical decoding on, a trigger per table and a small service |
| Undoes | One write at a time | One transaction, or a group of them | One whole agent run |
| Someone edited the same row later | Refuses, unless forced to overwrite | Refuses by default; can also undo their edit, or overwrite it | Keeps their edit, restores the rest, flags real clashes |
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
