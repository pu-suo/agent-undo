# Receipt format

A receipt is the record of what one agent run changed. `undo record` writes it, and `undo show`, `undo preview` and `undo apply` read it. This page specifies the files, the line format, and the order of writes that keeps receipts complete across crashes.

## Directory layout

The receipts directory (`--receipts`, default `./receipts`) holds:

| File | Contents |
| --- | --- |
| `<run>.jsonl` | One receipt per run, in JSON Lines |
| `state.json` | Which server, database and slot the receipts come from, and the last LSN acknowledged to the slot |
| `.lock` | An exclusive `flock`, so only one recorder writes the directory |

The directory must be mode 0700, and receipt files are created with mode 0600: receipts contain whole rows. A run ID must match `^run-[a-z0-9][a-z0-9._-]{0,58}$`. The tag trigger enforces the same rule, which also keeps run IDs safe as file names.

## Lines

Each line is one JSON object. A transaction is written as one `change` line per changed row, followed by one `commit` line.

```json
{"kind":"change","lsn":"0/BEE952D0","op":"update","relid":47479,"schema":"public","table":"customers","old":{"id":"1","name":"Customer 1","phone":"555-0001","plan":"pro"},"new":{"id":"1","name":"Customer 1","phone":"555-0001","plan":"cancelled"}}
{"kind":"change","lsn":"0/BEE95910","op":"delete","relid":47486,"schema":"public","table":"orders","old":{"customer_id":"2","id":"1","status":"paid","total":"7.99"}}
{"kind":"commit","xid":10878,"commit_lsn":"0/BEE95820","end_lsn":"0/BEE95850","time":"2026-11-12T01:12:11.730424Z","changes":10}
```

### change

| Field | Type | Meaning |
| --- | --- | --- |
| `kind` | `"change"` | |
| `lsn` | LSN | WAL position of the change, for reference. Not unique: every row of one multi-insert shares an LSN. |
| `op` | `insert`, `update`, `delete` or `truncate` | |
| `relid` | integer | The table's OID, which survives renames |
| `schema`, `table` | string | The table's name when the change was decoded |
| `old` | object | The whole row before an `update` or `delete` |
| `new` | object | The whole row after an `insert` or `update` |

`old` and `new` map column names to values. Each value is a string, or `null` for SQL NULL. Values are the text that PostgreSQL's output function produced. Every agent-undo connection pins the settings that affect that text (`DateStyle`, `IntervalStyle`, `TimeZone`, `extra_float_digits`, `bytea_output`, `client_encoding`), so undo can compare receipts with live rows as exact text. A large value that an update left unchanged is not repeated in the WAL; the recorder copies it from the old row. A `truncate` line has no rows, and undo refuses any run that contains one.

### commit

| Field | Type | Meaning |
| --- | --- | --- |
| `kind` | `"commit"` | |
| `xid` | integer | The transaction ID |
| `commit_lsn` | LSN | Position of the commit record |
| `end_lsn` | LSN | Position just past the commit record. This is the value acknowledged to the slot. |
| `time` | RFC 3339 | Commit time, in UTC |
| `changes` | integer | The number of change lines before it |
| `incomplete` | string, optional | Why some of the run's changes in this transaction may be missing, for example a table without `REPLICA IDENTITY FULL`. Undo refuses a run with an incomplete transaction. |

Attribution is per statement: a change belongs to the run named by the most recent tag in its transaction. A transaction that switched run IDs part way through therefore appears in more than one receipt, each with its own commit line.

LSNs are written as PostgreSQL prints them, for example `0/BEE95850`.

## state.json

```json
{
  "system_id": "7706603552223293478",
  "timeline": 1,
  "database": "shop",
  "slot": "undo_slot",
  "publication": "undo_pub",
  "agent_role": "agent_bot",
  "confirmed_lsn": "0/BEEB3C60",
  "gaps": [{"from": "0/A0000000", "to": "0/A1000000", "time": "2026-11-12T03:00:00Z"}]
}
```

`gaps` is present only after `undo init --new-epoch`. Each entry is a stretch of WAL that was never recorded. Undo refuses runs that started before a gap ended.

## Durability

The recorder acknowledges a position to the replication slot only when everything up to it is on disk. Each standby status update follows these steps, in this order:

1. Append the transaction's change lines and its commit line, and flush them to the operating system.
2. `fsync` every receipt written since the last update, and the directory if a file was created.
3. Write `state.json` with the new `confirmed_lsn`: write a temporary file, `fsync` it, rename it into place, and `fsync` the directory.
4. Send the status update with the write, flush and apply positions set to `confirmed_lsn`.

`confirmed_lsn` is the `end_lsn` of the last transaction handled. Between transactions, it is also the WAL end reported by the server's keepalive, so the slot advances while agents are idle. People's transactions are handled by skipping them.

## Recovery

- **A crash mid-write** can leave an unfinished transaction, or half a line, at the end of a receipt. Readers stop at the last commit line. The writer truncates the file to that point before its first append.
- **Duplicates.** After a restart, PostgreSQL resends everything after the slot's confirmed position. The recorder skips a transaction for a run when its `commit_lsn` is below the `end_lsn` of that run's last commit. A receipt left by a previous session is `fsync`ed before anything relies on it.
- **Corruption.** A line that can't be parsed, followed by a commit line, is damage rather than a torn write, and reading fails with the line number.
- **Wrong server or slot.** On start, the recorder checks the server's system ID and timeline against `state.json`. It refuses to start when the server's WAL ends before `confirmed_lsn` (a restored database), or when the slot's position is past it (the slot was moved or recreated). `undo init --new-epoch` records the gap and starts again.
