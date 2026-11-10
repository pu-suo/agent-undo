"""Undo for AI agents on a live Postgres database: a rough prototype.

Postgres already writes every change to its own log. This prototype reads that
log, keeps a receipt of what an agent changed, and later reverses only those
changes. Other people's changes stay in place. If someone changed the same value
after the agent, the row is flagged for a person to check.

Setup (Python 3.10+ and a throwaway local Postgres):

    pip install "psycopg[binary]"
    # In postgresql.conf set wal_level = logical, then restart Postgres
    createdb shop
    createdb shop_control
    export UNDO_DSN="host=127.0.0.1 dbname=shop client_encoding=utf8"
    export UNDO_CONTROL_DSN="host=127.0.0.1 dbname=shop_control client_encoding=utf8"
    python agent_undo_prototype.py demo

Only use throwaway databases. The demo drops and recreates its tables, and it logs
in as the roles postgres and agent_bot, so local trust authentication is simplest.

How attribution works: anything written by the database role `agent_bot` is
tagged in the log by a small statement trigger, so it works for any client.

Known limits:
  * It cannot see decisions that depended on the agent's changes, such as a person
    reading a wrong value, or an update that hit nothing because the row was gone.
  * DROP TABLE and TRUNCATE are not reversible here. Block those separately.
  * Emails, payments, and other outside actions cannot be undone.
  * Tables are given REPLICA IDENTITY FULL so the log keeps old values, which adds
    write overhead. Primary key changes are not handled.
"""
import json, os, random, re, sys, time
import psycopg

DSN = os.environ.get("UNDO_DSN", "host=127.0.0.1 dbname=shop client_encoding=utf8")
CONTROL_DSN = os.environ.get("UNDO_CONTROL_DSN", "host=127.0.0.1 dbname=shop_control client_encoding=utf8")
SLOT, AGENT_ROLE = "agent_undo", "agent_bot"
COL = re.compile(r"(\w+)\[[^\]]+\]:(null|'(?:[^']|'')*'|\S+)")
HEAD = re.compile(r"table (\w+)\.(\w+): (INSERT|UPDATE|DELETE): (.*)", re.S)

def row(text):
    out = {}
    for name, value in COL.findall(text):
        if value == "null": out[name] = None
        elif value.startswith("'"): out[name] = value[1:-1].replace("''", "'")
        else: out[name] = value
    return out

def parse(line):
    schema, table, op, body = HEAD.match(line).groups()
    if op == "UPDATE":
        old, new = body.split("new-tuple:", 1)
        return {"table": table, "op": op, "old": row(old.replace("old-key:", "")), "new": row(new)}
    if op == "DELETE": return {"table": table, "op": op, "old": row(body), "new": None}
    return {"table": table, "op": op, "old": None, "new": row(body)}

def install(c, tables):
    """Tag agent writes in the log and start keeping old values."""
    c.execute(f"""do $$ begin
        if not exists (select 1 from pg_roles where rolname = '{AGENT_ROLE}') then create role {AGENT_ROLE} login; end if;
    end $$""")
    c.execute(f"""create or replace function tag_agent_write() returns trigger language plpgsql security definer as $$
        begin
            if session_user = '{AGENT_ROLE}' then
                perform pg_logical_emit_message(true, 'agent_run', current_setting('application_name'));
            end if;
            return null;
        end $$""")
    c.execute(f"grant usage on schema public to {AGENT_ROLE}")
    for t in tables:
        c.execute(f"alter table {t} replica identity full")
        c.execute(f"grant select, insert, update, delete on {t} to {AGENT_ROLE}")
        c.execute(f"drop trigger if exists tag_agent on {t}")
        c.execute(f"create trigger tag_agent before insert or update or delete on {t} for each statement execute function tag_agent_write()")
    c.execute("select pg_drop_replication_slot(slot_name) from pg_replication_slots where slot_name = %s", (SLOT,))
    c.execute("select pg_create_logical_replication_slot(%s, 'test_decoding')", (SLOT,))

def read_log(c):
    """Return receipts: {run name: ordered list of row changes}."""
    receipts, cur = {}, None
    for (data,) in c.execute("select data from pg_logical_slot_get_changes(%s, null, null)", (SLOT,)).fetchall():
        if data.startswith("BEGIN"): cur = {"run": None, "changes": []}
        elif data.startswith("message:"): cur["run"] = data.split("content:", 1)[1]
        elif data.startswith("table "): cur["changes"].append(parse(data))
        elif data.startswith("COMMIT") and cur["run"]: receipts.setdefault(cur["run"], []).extend(cur["changes"])
    return receipts

def key_column(c, table):
    return c.execute("""select a.attname::text from pg_index i join pg_attribute a on a.attrelid = i.indrelid and a.attnum = any(i.indkey)
                        where i.indrelid = %s::regclass and i.indisprimary""", (table,)).fetchone()[0]

def current(c, table, key, value):
    cur = c.execute(f"select * from {table} where {key} = %s for update", (value,))
    names = [d.name for d in cur.description]
    r = cur.fetchone()
    return None if r is None else {n: None if v is None else str(v) for n, v in zip(names, r)}

def undo(c, changes):
    """Reverse one receipt, newest change first, in a single transaction."""
    restored, flagged = 0, []
    with c.transaction():
        for ch in reversed(changes):
            t, op, old, new = ch["table"], ch["op"], ch["old"], ch["new"]
            key = key_column(c, t)
            if op == "DELETE":
                if current(c, t, key, old[key]) is not None: flagged.append((t, old[key], "row exists again")); continue
                cols = list(old)
                c.execute(f"insert into {t} ({','.join(cols)}) values ({','.join(['%s'] * len(cols))})", [old[x] for x in cols]); restored += 1
            elif op == "INSERT":
                now = current(c, t, key, new[key])
                if now != new: flagged.append((t, new[key], "changed after the agent added it")); continue
                c.execute(f"delete from {t} where {key} = %s", (new[key],)); restored += 1
            else:
                now = current(c, t, key, new[key])
                if now is None: flagged.append((t, new[key], "row deleted later")); continue
                sets = {}
                for col in old:
                    if old[col] == new[col]: continue
                    if now[col] == new[col]: sets[col] = old[col]
                    else: flagged.append((t, new[key], f"{col} changed later"))
                if sets:
                    c.execute(f"update {t} set " + ",".join(f"{k} = %s" for k in sets) + f" where {key} = %s", list(sets.values()) + [new[key]])
                    restored += 1
    return restored, flagged

TABLES = ["customers", "orders"]
SCHEMA = """drop table if exists orders; drop table if exists customers;
    create table customers(id int primary key, name text, plan text not null, balance int not null);
    create table orders(id int primary key, customer_id int, status text not null);
    insert into customers select i, 'c' || i, case when i % 3 = 0 then 'trial' else 'paid' end, 100 from generate_series(1, 1000) i;
    insert into orders select i, 1 + (i % 1000), case when i % 4 = 0 then 'pending' else 'done' end from generate_series(1, 2000) i;"""

def person_ops(rng, n, next_order):
    """Ordinary work by people, aimed at specific rows. Pending orders are left alone
    so that no update silently misses a row the agent deleted (see the known limits)."""
    ops = []
    for _ in range(n):
        cid, kind = rng.randint(1, 1000), rng.random()
        if kind < .4: ops.append(f"update customers set balance = balance + 10 where id = {cid}")
        elif kind < .6: ops.append(f"update customers set name = 'renamed-{cid}' where id = {cid}")
        elif kind < .75: ops.append(f"update customers set plan = 'paid' where id = {cid}")
        elif kind < .9:
            next_order[0] += 1
            ops.append(f"insert into orders values ({next_order[0]}, {cid}, 'done')")
        else:
            ops.append(f"update orders set status = 'shipped' where id = {4 * rng.randint(0, 499) + 1}")
    return ops

AGENT = ["update customers set plan = 'cancelled' where plan = 'trial'",
         "update customers set balance = balance - 5 where plan = 'cancelled'",
         "delete from orders where status = 'pending'",
         "insert into orders values (999999, 1, 'bogus')"]

def snapshot(c, table):
    return {r[0]: tuple(map(str, r)) for r in c.execute(f"select * from {table}").fetchall()}

def trial(seed, admin, control, agent):
    rng = random.Random(seed); next_order = [3000]
    for c in (admin, control): c.execute(SCHEMA)
    install(admin, TABLES)
    for a in AGENT + [None]:
        for op in person_ops(rng, 30, next_order):
            admin.execute(op); control.execute(op)
        if a: agent.execute(a)
    receipts = read_log(admin)
    changes = receipts["run-42"]
    t0 = time.time(); restored, flagged = undo(admin, changes); ms = 1000 * (time.time() - t0)
    marked = {(t, str(k)) for t, k, _ in flagged}
    wrong = unexplained = 0
    for t in TABLES:
        a, b = snapshot(admin, t), snapshot(control, t)
        for k in set(a) | set(b):
            if a.get(k) != b.get(k):
                wrong += 1
                unexplained += (t, str(k)) not in marked
    return len(changes), restored, len(marked), wrong, unexplained, ms, changes

def demo(trials=30):
    admin = psycopg.connect(DSN + " user=postgres", autocommit=True); control = psycopg.connect(CONTROL_DSN + " user=postgres", autocommit=True)
    admin.execute("set client_min_messages = warning")
    admin.execute(SCHEMA); install(admin, TABLES)
    agent = psycopg.connect(DSN + f" user={AGENT_ROLE} application_name=run-42", autocommit=True)
    totals = dict(changes=0, restored=0, flagged=0, wrong=0, unexplained=0); slowest = 0
    for seed in range(trials):
        n, restored, marked, wrong, unexplained, ms, changes = trial(seed, admin, control, agent)
        totals["changes"] += n; totals["restored"] += restored; totals["flagged"] += marked
        totals["wrong"] += wrong; totals["unexplained"] += unexplained; slowest = max(slowest, ms)
    with open("run-42.receipt.jsonl", "w") as f:
        for ch in changes: f.write(json.dumps(ch) + "\n")
    print(f"{trials} trials, {totals['changes']} agent row changes in total, about {totals['changes'] // trials} per trial")
    print(f"restored {totals['restored']}, flagged for review {totals['flagged']}")
    print(f"rows that differ from the world where the agent never ran: {totals['wrong']}")
    print(f"of those, rows that were not flagged: {totals['unexplained']}")
    print(f"slowest undo: {slowest:.0f} ms. Last receipt saved to run-42.receipt.jsonl")

if __name__ == "__main__":
    if sys.argv[1:] == ["demo"]: demo()
    else: sys.exit(__doc__)
