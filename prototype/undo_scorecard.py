"""A starter scorecard of hard cases for agent_undo_prototype.py.

Each case runs twice: once with the agent, then undo, and once in a copy where the
agent never ran. PASS means both copies match. FLAGGED means undo asked a person to
look. WRONG means undo finished but the data is wrong. CRASH means undo failed.

Uses the same setup as agent_undo_prototype.py, including UNDO_DSN and
UNDO_CONTROL_DSN. Only use throwaway databases: it resets the public schema.
Keep this file next to agent_undo_prototype.py, then run: python undo_scorecard.py
"""
from collections import Counter
import psycopg
import agent_undo_prototype as U

BIG = "x" * 20000
# name, setup SQL, agent statements, people's statements, whether the agent owns the tables
CASES = [
 ("Deleting a parent also deletes its children",
  "create table t(id int primary key, v text); create table c(id int primary key, t_id int references t(id) on delete cascade); insert into t values (1,'a'),(2,'b'); insert into c values (1,1),(2,1),(3,2);",
  ["delete from t where id = 1"], [], False),
 ("A database trigger updates a summary table",
  """create table t(id int primary key, plan text); create table stats(plan text primary key, n int); insert into t values (1,'a'),(2,'a'); insert into stats values ('a',2),('b',0);
     create function bump() returns trigger language plpgsql security definer as $$ begin update stats set n=n-1 where plan=old.plan; update stats set n=n+1 where plan=new.plan; return new; end $$;
     create trigger bump after update on t for each row execute function bump();""",
  ["update t set plan='b' where id=1"], [], False),
 ("A restored row collides with a new unique value",
  "create table u(id int primary key, email text unique); insert into u values (1,'a@x');",
  ["delete from u where id=1"], ["insert into u values (2,'a@x')"], False),
 ("A restored row's parent was deleted by a person",
  "create table p(id int primary key); create table k(id int primary key, p_id int references p(id)); insert into p values (1); insert into k values (1,1);",
  ["delete from k where id=1"], ["delete from p where id=1"], False),
 ("A very large text value in the same row",
  f"create table t(id int primary key, plan text, body text); alter table t alter body set storage external; insert into t values (1,'a','{BIG}');",
  ["update t set plan='b' where id=1"], [], False),
 ("A stored value contains the log's own marker words",
  "create table t(id int primary key, v text, w text); insert into t values (1,'x new-tuple: y','keep');",
  ["update t set w='changed' where id=1"], [], False),
 ("JSON, booleans, decimals and timestamps",
  "create table t(id int primary key, j jsonb, b bool, n numeric(8,2), ts timestamptz, v text); insert into t values (1,'{\"a\": 1}',true,1.50,'2026-01-01 10:00:00+00','x');",
  ["update t set j='{\"a\": 2}', b=false, n=2.50, ts='2026-02-02 10:00:00+00', v='y' where id=1"], [], False),
 ("Arrays and binary data",
  "create table t(id int primary key, a text[], d bytea, v text); insert into t values (1,'{x,y}','\\xdead','o');",
  ["update t set a='{z}', d='\\xbeef' where id=1"], [], False),
 ("A computed (generated) column",
  "create table t(id int primary key, a int, b int generated always as (a*2) stored); insert into t(id,a) values (1,5);",
  ["delete from t where id=1"], [], False),
 ("The agent changes a primary key",
  "create table t(id int primary key, v text); insert into t values (1,'a');",
  ["update t set id=2 where id=1"], [], False),
 ("A table with no primary key",
  "create table t(a int, b text); insert into t values (1,'x'),(1,'x'),(2,'y');",
  ["delete from t where a=1"], [], False),
 ("A partitioned table",
  "create table t(id int, y int, v text, primary key(id,y)) partition by range(y); create table t_a partition of t for values from (0) to (10); create table t_b partition of t for values from (10) to (20); insert into t values (1,5,'a'),(2,15,'b');",
  ["update t set v='z' where id=1"], [], False),
 ("A person adds a column before undo",
  "create table t(id int primary key, v text); insert into t values (1,'a'),(2,'b');",
  ["delete from t where id=1", "update t set v='z' where id=2"], ["alter table t add column extra int default 7"], False),
 ("A person drops a column before undo",
  "create table t(id int primary key, v text, w text); insert into t values (1,'a','b');",
  ["delete from t where id=1"], ["alter table t drop column w"], False),
 ("The agent runs TRUNCATE",
  "create table t(id int primary key, v text); insert into t values (1,'a'),(2,'b');",
  ["truncate t"], [], True),
 ("The agent drops a table",
  "create table t(id int primary key, v text); insert into t values (1,'a'),(2,'b');",
  ["drop table t"], [], True),
]

def user_tables(c):
    return [r[0] for r in c.execute("""select c.relname::text from pg_class c join pg_namespace n on n.oid = c.relnamespace
        where n.nspname = 'public' and c.relkind in ('r', 'p') and not c.relispartition""").fetchall()]

def state(c, tables):
    out = {}
    for t in tables:
        try: out[t] = sorted(map(repr, c.execute(f"select * from {t}").fetchall()))
        except psycopg.Error: out[t] = "missing"
    return out

def run_case(setup, agent_sql, person_sql, owned):
    main = psycopg.connect(U.DSN + " user=postgres", autocommit=True)
    control = psycopg.connect(U.CONTROL_DSN + " user=postgres", autocommit=True)
    for c in (main, control):
        c.execute("drop schema public cascade"); c.execute("create schema public")
        c.execute("set client_min_messages = warning"); c.execute(setup.encode())
    tables = user_tables(main)
    U.install(main, tables)
    if owned:
        for t in tables: main.execute(f"alter table {t} owner to {U.AGENT_ROLE}")
    agent = psycopg.connect(U.DSN + f" user={U.AGENT_ROLE} application_name=run-42", autocommit=True)
    try:
        for q in agent_sql: agent.execute(q)
        for q in person_sql: main.execute(q); control.execute(q)
        restored, flagged = U.undo(main, U.read_log(main).get("run-42", []))
        if state(main, tables) == state(control, tables): return "PASS"
        return "FLAGGED" if flagged else "WRONG"
    except Exception as e:
        return f"CRASH ({type(e).__name__})"
    finally:
        main.execute("select pg_drop_replication_slot(slot_name) from pg_replication_slots where slot_name = %s", (U.SLOT,))
        for c in (main, control, agent): c.close()

if __name__ == "__main__":
    tally = Counter()
    for name, setup, agent_sql, person_sql, owned in CASES:
        result = run_case(setup, agent_sql, person_sql, owned)
        tally[result.split()[0]] += 1
        print(f"{result:32} {name}")
    print(dict(tally))
