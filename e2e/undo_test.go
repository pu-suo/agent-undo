package e2e

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pu-suo/agent-undo/internal/db"
	"github.com/pu-suo/agent-undo/internal/undo"
)

// snapshot returns every row of every table in the public schema, as text,
// in a stable order.
func snapshot(t *testing.T, d *testDB) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, table := range d.query(t, `select tablename from pg_tables where schemaname = 'public' order by 1`) {
		ident := pgx.Identifier{table}.Sanitize()
		out[table] = d.query(t, `select r::text from `+ident+` r order by 1`)
	}
	return out
}

func diff(a, b map[string][]string) []string {
	var out []string
	for table, rows := range a {
		if !slices.Equal(rows, b[table]) {
			out = append(out, table+": "+strings.Join(rows, " ")+"  vs  "+strings.Join(b[table], " "))
		}
	}
	for table := range b {
		if _, ok := a[table]; !ok {
			out = append(out, table+": missing")
		}
	}
	slices.Sort(out)
	return out
}

func (d *testDB) undoConn(t *testing.T) *pgx.Conn {
	t.Helper()
	conn, err := db.Connect(context.Background(), d.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

func (d *testDB) preview(t *testing.T, run string) *undo.Plan {
	t.Helper()
	plan, err := undo.Preview(context.Background(), d.undoConn(t), undo.Options{Receipts: d.receipts, Run: run})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	return plan
}

func (d *testDB) apply(t *testing.T, run, hash string) (*undo.Plan, error) {
	t.Helper()
	return undo.Apply(context.Background(), d.undoConn(t), undo.Options{
		Receipts: d.receipts, Run: run, PlanHash: hash, Wait: 20 * time.Second,
	})
}

// demoSchema mirrors sql/1_setup.sql: 20 customers, 40 orders.
const demoSchema = `
create table customers (id int primary key, name text not null, plan text not null, phone text);
create table orders (id int primary key, customer_id int not null references customers(id),
                     total numeric(10,2) not null, status text not null);
insert into customers select g, 'Customer ' || g, case when g % 3 = 0 then 'trial' else 'pro' end,
                             '555-' || lpad(g::text, 4, '0') from generate_series(1, 20) g;
insert into orders select g, 1 + (g % 20), (g * 7 % 100) + 0.99, 'paid' from generate_series(1, 40) g;
grant select, insert, update, delete on customers, orders to undo_test_agent;
`

// The README demo: the agent cancels customers 1 to 10 and deletes their
// orders, while support edits customers. Undo must restore the agent's
// changes, keep support's, and flag the one clash.
func TestUndoTheDemo(t *testing.T) {
	twin := newTwin(t, demoSchema)
	d := newDB(t, demoSchema)
	d.startRecorder(t)

	if err := d.agent(t, "run-42",
		`update customers set plan = 'cancelled' where id <= 10`,
		`delete from orders where customer_id in (select id from customers where plan = 'cancelled')`,
	); err != nil {
		t.Fatal(err)
	}
	support := []string{
		`update customers set phone = '555-9999' where id = 2`, // another column of a row the agent changed
		`update customers set plan = 'basic' where id = 4`,     // the same column: undo must keep it
		`update customers set plan = 'pro' where id = 15`,      // a row the agent never touched
	}
	for _, sql := range support {
		d.exec(t, sql)
		twin.exec(t, sql)
	}
	d.waitCaughtUp(t)
	before := snapshot(t, d)

	plan := d.preview(t, "run-42")
	if !slices.Equal(diff(before, snapshot(t, d)), nil) {
		t.Fatal("preview changed the database")
	}
	var kept []string
	for _, r := range plan.Rows {
		if r.Action == undo.Keep {
			kept = append(kept, r.Table+" "+r.Key)
		}
	}
	if strings.Join(kept, ",") != "public.customers id=4" {
		t.Errorf("rows kept for a person = %v, want only customer 4", kept)
	}

	applied, err := d.apply(t, "run-42", plan.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if !applied.Applied || !applied.NeedsPerson() {
		t.Errorf("apply: applied=%v needsPerson=%v, want both", applied.Applied, applied.NeedsPerson())
	}
	if d := diff(snapshot(t, d), snapshot(t, twin)); len(d) > 0 {
		t.Errorf("after undo the database differs from the twin:\n%s", strings.Join(d, "\n"))
	}

	// The run is sealed now: the agent can't keep going with it.
	if err := d.agent(t, "run-42", `update customers set plan = 'x' where id = 1`); err == nil || !strings.Contains(err.Error(), "sealed for undo") {
		t.Errorf("write from a sealed run = %v, want it refused", err)
	}
	// A second undo finds nothing left to do.
	again := d.preview(t, "run-42")
	for _, r := range again.Rows {
		if r.Action == undo.Restore || r.Action == undo.Partial {
			t.Errorf("second preview still wants to %s %s %s", r.Write, r.Table, r.Key)
		}
	}
}

// Apply refuses to act on a plan that is not the one that was previewed.
func TestApplyNeedsTheSamePlan(t *testing.T) {
	d := newDB(t, demoSchema)
	d.startRecorder(t)
	if err := d.agent(t, "run-1", `update customers set plan = 'x' where id = 1`); err != nil {
		t.Fatal(err)
	}
	d.waitCaughtUp(t)
	plan := d.preview(t, "run-1")
	d.exec(t, `update customers set plan = 'y' where id = 1`) // the row changes after the preview
	if _, err := d.apply(t, "run-1", plan.Hash); !errors.Is(err, undo.ErrPlanChanged) {
		t.Errorf("apply after the row changed = %v, want ErrPlanChanged", err)
	}
	if got := d.query(t, `select plan from customers where id = 1`)[0]; got != "y" {
		t.Errorf("plan = %q, want the person's 'y' untouched", got)
	}
}

// A person edits a row between two of the run's transactions. Undo must
// reverse the agent's later change but keep the person's value rather than
// jumping back to the value from before the run.
func TestEditBetweenTwoAgentTransactions(t *testing.T) {
	twin := newTwin(t, demoSchema)
	d := newDB(t, demoSchema)
	d.startRecorder(t)
	if err := d.agent(t, "run-5", `update customers set plan = 'cancelled' where id = 1`); err != nil {
		t.Fatal(err)
	}
	d.exec(t, `update customers set plan = 'basic' where id = 1`)
	twin.exec(t, `update customers set plan = 'basic' where id = 1`)
	if err := d.agent(t, "run-5", `update customers set plan = 'trial' where id = 1`); err != nil {
		t.Fatal(err)
	}
	d.waitCaughtUp(t)
	plan := d.preview(t, "run-5")
	if _, err := d.apply(t, "run-5", plan.Hash); err != nil {
		t.Fatal(err)
	}
	if got := d.query(t, `select plan from customers where id = 1`)[0]; got != "basic" {
		t.Errorf("plan = %q, want the person's 'basic'", got)
	}
	if d := diff(snapshot(t, d), snapshot(t, twin)); len(d) > 0 {
		t.Errorf("differs from the twin:\n%s", strings.Join(d, "\n"))
	}
}
