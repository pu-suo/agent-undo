package e2e

import (
	"strings"
	"testing"

	"github.com/pu-suo/agent-undo/internal/receipt"
)

// shopSchema is a small version of the demo shop. The agent may change
// customers and orders; audit is a table it cannot touch.
const shopSchema = `
create table customers (id int primary key, name text not null, plan text not null, phone text);
create table orders (
    id int primary key,
    customer_id int not null references customers(id),
    total numeric(10,2) not null,
    status text not null
);
insert into customers
select g, 'Customer ' || g, case when g % 3 = 0 then 'trial' else 'pro' end, '555-' || lpad(g::text, 4, '0')
from generate_series(1, 10) g;
insert into orders select g, 1 + (g % 10), (g * 7 % 100) + 0.99, 'paid' from generate_series(1, 20) g;
grant select, insert, update, delete on customers, orders to undo_test_agent;
`

func TestRecordsOnlyTheAgentsChanges(t *testing.T) {
	d := newDB(t, shopSchema)
	stop := d.startRecorder(t)

	if err := d.agent(t, "run-42",
		`update customers set plan = 'cancelled' where id <= 3`,
		`delete from orders where customer_id = 2`,
	); err != nil {
		t.Fatal(err)
	}
	d.exec(t, `update customers set phone = '555-9999' where id = 2`) // a person, not the agent
	d.waitCaughtUp(t)
	stop()

	txns := d.read(t, "run-42")
	if len(txns) != 2 {
		t.Fatalf("got %d transactions, want 2", len(txns))
	}
	cancel, deletes := txns[0].Changes, txns[1].Changes
	if len(cancel) != 3 || cancel[0].Op != receipt.Update || *cancel[0].Old["plan"] != "pro" || *cancel[0].New["plan"] != "cancelled" {
		t.Errorf("first transaction = %+v, want three plan updates", cancel)
	}
	if len(deletes) != 2 || deletes[0].Op != receipt.Delete || *deletes[0].Old["customer_id"] != "2" {
		t.Errorf("second transaction = %+v, want the two orders of customer 2 deleted", deletes)
	}
	for _, txn := range txns {
		for _, c := range txn.Changes {
			if c.New != nil && c.New["phone"] != nil && *c.New["phone"] == "555-9999" {
				t.Error("the person's edit was recorded as the agent's")
			}
		}
	}
}

// Changes made while the recorder is down wait in the slot. After a restart
// they are recorded exactly once.
func TestRestartRecordsEverythingOnce(t *testing.T) {
	d := newDB(t, shopSchema)
	stop := d.startRecorder(t)
	if err := d.agent(t, "run-7", `update customers set plan = 'basic' where id = 1`); err != nil {
		t.Fatal(err)
	}
	d.waitCaughtUp(t)
	stop()

	if err := d.agent(t, "run-7", `update customers set plan = 'trial' where id = 1`); err != nil {
		t.Fatal(err)
	}
	stop = d.startRecorder(t)
	d.waitCaughtUp(t)
	stop()
	stop = d.startRecorder(t) // a restart with nothing new must add nothing
	d.waitCaughtUp(t)
	stop()

	txns := d.read(t, "run-7")
	if len(txns) != 2 || *txns[1].Changes[0].New["plan"] != "trial" {
		t.Fatalf("got %d transactions, want the two updates once each", len(txns))
	}
}

func TestTriggerRejectsBadRunIDs(t *testing.T) {
	d := newDB(t, shopSchema)
	err := d.agent(t, "psql", `update customers set plan = 'x' where id = 1`)
	if err == nil || !strings.Contains(err.Error(), "not a valid run ID") {
		t.Errorf("agent write without a run ID = %v, want it rejected", err)
	}
	if err := d.agent(t, "run-1", `select 1`); err != nil {
		t.Errorf("a read-only statement should not need a tag: %v", err)
	}
}

// Statements in one transaction can belong to different runs when the agent
// switches its run ID; each run gets exactly its own changes.
func TestRunSwitchInsideATransaction(t *testing.T) {
	d := newDB(t, shopSchema)
	stop := d.startRecorder(t)
	err := d.agent(t, "run-a", `begin`,
		`update customers set plan = 'a' where id = 1`,
		`set application_name = 'run-b'`,
		`update customers set plan = 'b' where id = 2`,
		`commit`)
	if err != nil {
		t.Fatal(err)
	}
	d.waitCaughtUp(t)
	stop()
	a, b := d.read(t, "run-a"), d.read(t, "run-b")
	if len(a) != 1 || len(b) != 1 || len(a[0].Changes) != 1 || len(b[0].Changes) != 1 ||
		*a[0].Changes[0].New["id"] != "1" || *b[0].Changes[0].New["id"] != "2" {
		t.Errorf("run-a = %+v, run-b = %+v", a, b)
	}
}

func TestCheckFindsUntrackedTables(t *testing.T) {
	d := newDB(t, shopSchema)
	// A table granted after init is not tagged until init runs again.
	d.exec(t, `create table notes (id int primary key, body text); grant insert on notes to undo_test_agent`)
	findings := check(t, d)
	if !strings.Contains(strings.Join(findings, "\n"), "public.notes is not tagged") {
		t.Errorf("check missed the untracked table:\n%s", strings.Join(findings, "\n"))
	}
}
