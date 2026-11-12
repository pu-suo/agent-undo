package e2e

import (
	"errors"
	"strings"
	"testing"

	"github.com/pu-suo/agent-undo/internal/receipt"
	"github.com/pu-suo/agent-undo/internal/undo"
)

// hardCase is one of the 16 hard cases from prototype/undo_scorecard.py.
type hardCase struct {
	name   string
	setup  string   // run as an admin in both databases
	agent  []string // run as the agent, in the recorded database only
	people []string // run as a person in both databases
	want   string   // "same": undo matches the twin; "flagged": undo asks a person
}

// allTables lets the agent write every table in the schema.
const allTables = `grant select, insert, update, delete on all tables in schema public to undo_test_agent;`

var hardCases = []hardCase{
	{
		name: "deleting a parent also deletes its children",
		setup: `create table t(id int primary key, v text);
			create table c(id int primary key, t_id int references t(id) on delete cascade);
			insert into t values (1, 'a'), (2, 'b'); insert into c values (1, 1), (2, 1), (3, 2);` + allTables,
		agent: []string{`delete from t where id = 1`},
		want:  "same",
	},
	{
		name: "a database trigger updates a summary table",
		setup: `create table t(id int primary key, plan text); create table stats(plan text primary key, n int);
			insert into t values (1, 'a'), (2, 'a'); insert into stats values ('a', 2), ('b', 0);
			create function bump() returns trigger language plpgsql security definer set search_path = public as $$
			begin update stats set n = n - 1 where plan = old.plan; update stats set n = n + 1 where plan = new.plan; return new; end $$;
			create trigger bump after update on t for each row execute function bump();` + allTables,
		agent: []string{`update t set plan = 'b' where id = 1`},
		want:  "flagged", // refused: the trigger would fire again during undo
	},
	{
		name:   "a restored row collides with a new unique value",
		setup:  `create table u(id int primary key, email text unique); insert into u values (1, 'a@x');` + allTables,
		agent:  []string{`delete from u where id = 1`},
		people: []string{`insert into u values (2, 'a@x')`},
		want:   "flagged",
	},
	{
		name: "a restored row's parent was deleted by a person",
		setup: `create table p(id int primary key); create table k(id int primary key, p_id int references p(id));
			insert into p values (1); insert into k values (1, 1);` + allTables,
		agent:  []string{`delete from k where id = 1`},
		people: []string{`delete from p where id = 1`},
		want:   "flagged",
	},
	{
		name: "a very large text value in the same row",
		setup: `create table t(id int primary key, plan text, body text); alter table t alter body set storage external;
			insert into t values (1, 'a', repeat('x', 20000));` + allTables,
		agent: []string{`update t set plan = 'b' where id = 1`},
		want:  "same",
	},
	{
		name:  "a stored value contains the log's own marker words",
		setup: `create table t(id int primary key, v text, w text); insert into t values (1, 'x new-tuple: y', 'keep');` + allTables,
		agent: []string{`update t set w = 'changed' where id = 1`},
		want:  "same",
	},
	{
		name: "JSON, booleans, decimals and timestamps",
		setup: `create table t(id int primary key, j jsonb, b bool, n numeric(8,2), ts timestamptz, v text);
			insert into t values (1, '{"a": 1}', true, 1.50, '2026-01-01 10:00:00+00', 'x');` + allTables,
		agent: []string{`update t set j = '{"a": 2}', b = false, n = 2.50, ts = '2026-02-02 10:00:00+00', v = 'y' where id = 1`},
		want:  "same",
	},
	{
		name:  "arrays and binary data",
		setup: `create table t(id int primary key, a text[], d bytea, v text); insert into t values (1, '{x,y}', '\xdead', 'o');` + allTables,
		agent: []string{`update t set a = '{z}', d = '\xbeef' where id = 1`},
		want:  "same",
	},
	{
		name:  "a computed (generated) column",
		setup: `create table t(id int primary key, a int, b int generated always as (a * 2) stored); insert into t(id, a) values (1, 5);` + allTables,
		agent: []string{`delete from t where id = 1`},
		want:  "same",
	},
	{
		name:  "the agent changes a primary key",
		setup: `create table t(id int primary key, v text); insert into t values (1, 'a');` + allTables,
		agent: []string{`update t set id = 2 where id = 1`},
		want:  "flagged", // refused in v1
	},
	{
		name:  "a table with no primary key",
		setup: `create table t(a int, b text); insert into t values (1, 'x'), (1, 'x'), (2, 'y');` + allTables,
		agent: []string{`delete from t where a = 1`},
		want:  "flagged", // refused: the rows can't be identified
	},
	{
		name: "a partitioned table",
		setup: `create table t(id int, y int, v text, primary key (id, y)) partition by range (y);
			create table t_a partition of t for values from (0) to (10);
			create table t_b partition of t for values from (10) to (20);
			insert into t values (1, 5, 'a'), (2, 15, 'b');` + allTables,
		agent: []string{`update t set v = 'z' where id = 1`},
		want:  "same",
	},
	{
		name:   "a person adds a column before undo",
		setup:  `create table t(id int primary key, v text); insert into t values (1, 'a'), (2, 'b');` + allTables,
		agent:  []string{`delete from t where id = 1`, `update t set v = 'z' where id = 2`},
		people: []string{`alter table t add column extra int default 7`},
		want:   "same",
	},
	{
		name:   "a person drops a column before undo",
		setup:  `create table t(id int primary key, v text, w text); insert into t values (1, 'a', 'b');` + allTables,
		agent:  []string{`delete from t where id = 1`},
		people: []string{`alter table t drop column w`},
		want:   "same",
	},
	{
		name:  "the agent runs TRUNCATE",
		setup: `create table t(id int primary key, v text); insert into t values (1, 'a'), (2, 'b');` + allTables + `grant truncate on t to undo_test_agent;`,
		agent: []string{`truncate t`},
		want:  "flagged", // refused: the WAL does not keep truncated rows
	},
	{
		name:  "the agent drops a table",
		setup: `create table t(id int primary key, v text); insert into t values (1, 'a'), (2, 'b');` + allTables,
		agent: []string{`drop table t`}, // fails: the agent owns nothing, so it can't drop
		want:  "same",
	},
}

func TestHardCases(t *testing.T) {
	for _, hc := range hardCases {
		t.Run(hc.name, func(t *testing.T) {
			twin := newTwin(t, hc.setup)
			d := newDB(t, hc.setup)
			d.startRecorder(t)

			for _, sql := range hc.agent {
				if err := d.agent(t, "run-42", sql); err != nil {
					t.Logf("the agent's %q failed: %v", sql, err)
				}
			}
			for _, sql := range hc.people {
				d.exec(t, sql)
				twin.tryExec(t, sql) // may fail in the twin, where the agent never ran
			}
			d.waitCaughtUp(t)

			plan, err := undo.Preview(t.Context(), d.undoConn(t), undo.Options{Receipts: d.receipts, Run: "run-42"})
			switch {
			case errors.Is(err, receipt.ErrNotFound):
				plan = &undo.Plan{} // the agent changed nothing
			case err != nil:
				t.Fatalf("preview: %v", err)
			case plan.Refused == "" && len(plan.Rows) > 0:
				if plan, err = d.apply(t, "run-42", plan.Hash); err != nil {
					t.Fatalf("apply: %v", err)
				}
			}

			differences := diff(snapshot(t, d), snapshot(t, twin))
			got := "same"
			switch {
			case len(differences) > 0 && plan.NeedsPerson():
				got = "flagged"
			case len(differences) > 0:
				t.Fatalf("WRONG: undo finished, flagged nothing, and the data differs from the twin:\n%s",
					strings.Join(differences, "\n"))
			}
			if got != hc.want {
				t.Errorf("got %s, want %s (refused: %q)", got, hc.want, plan.Refused)
			}
			t.Logf("%s; refused: %q", got, plan.Refused)
		})
	}
}

// tryExec runs SQL as a person and ignores errors.
func (d *testDB) tryExec(t *testing.T, sql string) {
	t.Helper()
	conn := d.connect(t, d.userConfig())
	defer conn.Close(t.Context())
	if _, err := conn.PgConn().Exec(t.Context(), sql).ReadAll(); err != nil {
		t.Logf("in the twin, %q failed: %v", sql, err)
	}
}
