package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/pu-suo/agent-undo/internal/db"
	"github.com/pu-suo/agent-undo/internal/receipt"
	"github.com/pu-suo/agent-undo/internal/undo"
)

// The randomized differential test. Each run builds a small shop with random
// data, then mixes random agent transactions with random edits by people.
// People's edits go to the recorded database and to a control database; the
// agent's go only to the recorded one. After undo, the two must match except
// where undo reported something: a conflict on that column, a conflict on the
// whole row, or a refusal. Any other difference is a silent error and fails
// the test.
//
// People's edits follow two rules that keep the control a fair answer key:
//   - People write rows that exist in both databases, or rows the agent
//     inserted (the control lacks those, so there the edit changes nothing).
//     They never write a row the agent deleted: they couldn't see it.
//   - People never write a value the agent wrote. Undo compares values, so it
//     can't tell the two apart (a documented limit).
//
// Settings:
//
//	AGENT_UNDO_RANDOM_RUNS  how many runs (default 20)
//	AGENT_UNDO_RANDOM_SEED  the first run's seed (default 1); run i uses seed+i

const randomRunID = "run-random"

const randomSchema = `
create table customers (id int primary key, plan text not null, phone text, note text,
                        score int, tags jsonb, body text);
alter table customers alter body set storage external; -- large values are stored out of line
create table orders (id int primary key, customer_id int not null references customers(id) on delete cascade,
                     status text not null, total numeric(10,2) not null);
grant select, insert, update, delete on customers, orders to undo_test_agent;
`

var (
	// Columns an update may set; repeats make a column more likely.
	customerColumns = []string{"plan", "plan", "phone", "phone", "note", "score", "tags", "body"}
	orderColumns    = []string{"status", "total"}
)

func TestRandomRuns(t *testing.T) {
	if testing.Short() {
		t.Skip("needs Postgres; skipped with -short")
	}
	runs := envInt(t, "AGENT_UNDO_RANDOM_RUNS", 20)
	first := envInt(t, "AGENT_UNDO_RANDOM_SEED", 1)

	// Create the agent role once, before the runs start in parallel.
	ctx := context.Background()
	adminCfg, err := db.Config(adminURL())
	if err != nil {
		t.Fatal(err)
	}
	admin, err := db.Connect(ctx, adminCfg)
	if err != nil {
		t.Fatalf("%v\n(start the database with: docker compose up -d)", err)
	}
	ensureAgentRole(t, admin)
	admin.Close(ctx)

	total := &randomTotals{}
	t.Run("runs", func(t *testing.T) {
		for seed := first; seed < first+runs; seed++ {
			t.Run("seed="+strconv.Itoa(seed), func(t *testing.T) {
				t.Parallel()
				total.add(randomRun(t, uint64(seed)))
			})
		}
	})

	s := &total.randomStats
	t.Logf(`random runs: %d finished (seeds %d to %d)
agent: %d transactions (%d rolled back), %d statements, %d row changes recorded
people: %d statements
undo plans: %d rows: %d restore, %d partial, %d keep, %d nothing, %d refuse; %d whole runs refused
after undo: %d of %d runs match the control exactly; %d rows differ from it, %d of them reported
silent errors: %d
conflicts on rows no person wrote: %d`,
		s.runs, first, first+runs-1,
		s.agentTxns, s.agentFailed, s.agentStatements, s.changes,
		s.peopleStatements,
		s.planRows(), s.actions[undo.Restore], s.actions[undo.Partial], s.actions[undo.Keep],
		s.actions[undo.Nothing], s.actions[undo.Refuse], s.refusedRuns,
		s.exactRuns, s.runs, s.differing, s.reported,
		s.silent,
		s.unexplained)
	for _, e := range total.examples[:min(len(total.examples), 10)] {
		t.Logf("  %s", e)
	}
}

// randomRun runs one seed: random data, a random mix of agent transactions
// and people's edits, then undo and the comparison with the control.
func randomRun(t *testing.T, seed uint64) *randomStats {
	g := &generator{rng: rand.New(rand.NewPCG(seed, 0)), agentRows: map[string]bool{}, touched: map[string]bool{}}
	schema := g.schema()
	control := newControl(t, schema)
	d := newDB(t, schema)
	d.startRecorder(t)
	s := &randomStats{runs: 1, actions: map[undo.Action]int{}}
	// One connection each for the whole run: thousands of runs that opened a
	// connection per query would use up the client's local ports.
	rec, ctl, agent := openConn(t, d), openConn(t, control), d.agentConn(t, randomRunID)

	steps := 6 + g.rng.IntN(11)
	agentStep := g.rng.IntN(steps) // so every run has at least one agent transaction
	for i := range steps {
		if i == agentStep || g.rng.IntN(5) < 2 {
			statements := g.agentTransaction(t, rec)
			s.agentTxns++
			s.agentStatements += len(statements)
			g.log("agent:  begin; " + strings.Join(statements, "; ") + "; commit")
			if err := runTransaction(t.Context(), agent, statements); err != nil {
				s.agentFailed++
				g.log("        (rolled back: " + err.Error() + ")")
			}
		} else {
			for range 1 + g.rng.IntN(3) {
				sql := g.personStatement(t, rec, ctl)
				s.peopleStatements++
				g.log("person: " + sql)
				execSQL(t, rec, sql)
				execSQL(t, ctl, sql)
			}
		}
	}
	d.waitCaughtUp(t)
	for _, txn := range d.read(t, randomRunID) {
		s.changes += len(txn.Changes)
	}

	plan, err := undo.Preview(t.Context(), d.undoConn(t), undo.Options{Receipts: d.receipts, Run: randomRunID})
	switch {
	case errors.Is(err, receipt.ErrNotFound):
		plan = &undo.Plan{} // every agent transaction rolled back
	case err != nil:
		t.Fatalf("preview: %v\n%s", err, g.history(seed))
	case plan.Refused != "":
		s.refusedRuns++
	case len(plan.Rows) > 0:
		if plan, err = d.apply(t, randomRunID, plan.Hash); err != nil {
			t.Fatalf("apply: %v\n%s", err, g.history(seed))
		}
	}

	byKey := map[string]*undo.RowPlan{}
	for i := range plan.Rows {
		r := &plan.Rows[i]
		name := r.Table + " " + r.Key
		byKey[name] = r
		s.actions[r.Action]++
		if (r.Action == undo.Keep || r.Action == undo.Partial) && !g.touched[name] {
			s.unexplained++
			s.examples = append(s.examples, fmt.Sprintf("seed %d: %s %s: %s", seed, r.Action, name,
				strings.Join(append(slices.Clone(r.Conflicts), r.Reason), "; ")))
			t.Logf("conflict on %s, a row no person wrote:\n%s", name, g.history(seed))
		}
	}

	for _, table := range []string{"customers", "orders"} {
		got, want := rowsByKey(t, rec, table), rowsByKey(t, ctl, table)
		for _, key := range slices.Sorted(maps.Keys(union(got, want))) {
			if len(differingColumns(got[key], want[key])) == 0 && (got[key] == nil) == (want[key] == nil) {
				continue
			}
			s.differing++
			name := "public." + table + " id=" + key
			why := unreported(plan, byKey[name], got[key], want[key])
			if why == "" {
				s.reported++
				continue
			}
			s.silent++
			t.Errorf("SILENT ERROR in %s: %s\n  after undo: %s\n  control:    %s\n  plan:       %+v\n%s",
				name, why, show(got[key]), show(want[key]), byKey[name], g.history(seed))
		}
	}
	if s.differing == 0 {
		s.exactRuns = 1
	}
	return s
}

// unreported says why a row that differs from the control was not reported
// by undo, or returns "" if undo did report it.
func unreported(plan *undo.Plan, r *undo.RowPlan, got, want map[string]json.RawMessage) string {
	switch {
	case plan.Refused != "":
		return "" // undo refused the whole run, and said so
	case r == nil:
		return "the row is not in the undo plan"
	case r.Action == undo.Refuse:
		return ""
	case r.Action == undo.Restore || r.Action == undo.Nothing:
		return fmt.Sprintf("undo planned %q, which reports no conflict", r.Action)
	}
	// keep or partial: a reason or a row conflict covers the whole row; a
	// column conflict covers only its column.
	if r.Reason != "" || slices.ContainsFunc(r.Conflicts, func(c string) bool { return !isColumnConflict(c) }) {
		return ""
	}
	if got == nil || want == nil {
		return "the row exists in only one database, but undo reported only column conflicts"
	}
	for _, col := range differingColumns(got, want) {
		if !slices.ContainsFunc(r.Conflicts, func(c string) bool { return strings.HasPrefix(c, col+": ") }) {
			return "column " + col + " differs, but undo reported no conflict for it"
		}
	}
	return ""
}

// isColumnConflict reports whether c names one column, as in
// "plan: someone changed it to ...".
func isColumnConflict(c string) bool {
	return strings.Contains(c, ": someone changed it to ")
}

// openConn opens a connection as a person, for the rest of the test.
func openConn(t *testing.T, d *testDB) *pgx.Conn {
	t.Helper()
	conn := d.connect(t, d.userConfig())
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// column returns the first column of every row, as text.
func column(t *testing.T, conn *pgx.Conn, sql string) []string {
	t.Helper()
	rows, err := conn.Query(t.Context(), sql, pgx.QueryResultFormats{pgx.TextFormatCode})
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return out
}

func execSQL(t *testing.T, conn *pgx.Conn, sql string) {
	t.Helper()
	if _, err := conn.Exec(t.Context(), sql); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// runTransaction runs statements in one transaction, and rolls it back if
// one of them fails.
func runTransaction(ctx context.Context, conn *pgx.Conn, statements []string) error {
	if _, err := conn.Exec(ctx, "begin"); err != nil {
		return err
	}
	for _, sql := range statements {
		if _, err := conn.Exec(ctx, sql); err != nil {
			_, _ = conn.Exec(ctx, "rollback")
			return err
		}
	}
	_, err := conn.Exec(ctx, "commit")
	return err
}

// rowsByKey reads a table as JSON rows keyed by id.
func rowsByKey(t *testing.T, conn *pgx.Conn, table string) map[string]map[string]json.RawMessage {
	t.Helper()
	out := map[string]map[string]json.RawMessage{}
	for _, line := range column(t, conn, `select id::text || ' ' || to_jsonb(r)::text from `+table+` r`) {
		key, doc, _ := strings.Cut(line, " ")
		var row map[string]json.RawMessage
		if err := json.Unmarshal([]byte(doc), &row); err != nil {
			t.Fatal(err)
		}
		out[key] = row
	}
	return out
}

func union(a, b map[string]map[string]json.RawMessage) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

// differingColumns lists the columns whose values differ between two rows
// that both exist.
func differingColumns(a, b map[string]json.RawMessage) []string {
	if a == nil || b == nil {
		return nil
	}
	var out []string
	cols := map[string]bool{}
	for col := range a {
		cols[col] = true
	}
	for col := range b {
		cols[col] = true
	}
	for _, col := range slices.Sorted(maps.Keys(cols)) {
		if !bytes.Equal(a[col], b[col]) {
			out = append(out, col)
		}
	}
	return out
}

// show prints a row, shortening long values.
func show(row map[string]json.RawMessage) string {
	if row == nil {
		return "(no row)"
	}
	var parts []string
	for _, col := range slices.Sorted(maps.Keys(row)) {
		v := string(row[col])
		if len(v) > 24 {
			v = v[:12] + "..." + strconv.Itoa(len(v)) + " bytes"
		}
		parts = append(parts, col+"="+v)
	}
	return strings.Join(parts, " ")
}

// generator makes the random data and statements for one run.
type generator struct {
	rng                         *rand.Rand
	n                           int             // makes every written value unique
	agentCustomer, agentOrder   int             // the last ids the agent inserted
	personCustomer, personOrder int             // the last ids people inserted
	agentRows                   map[string]bool // rows the agent targeted, so people's edits can aim at them
	touched                     map[string]bool // rows people wrote, directly or through a cascade
	lines                       []string
}

type order struct{ id, customer int }

func cust(id int) string { return "public.customers id=" + strconv.Itoa(id) }
func ord(id int) string  { return "public.orders id=" + strconv.Itoa(id) }

func (g *generator) log(line string) { g.lines = append(g.lines, line) }

func (g *generator) history(seed uint64) string {
	return fmt.Sprintf("reproduce with: AGENT_UNDO_RANDOM_SEED=%d AGENT_UNDO_RANDOM_RUNS=1 go test ./e2e -run TestRandomRuns -v\n  %s",
		seed, strings.Join(g.lines, "\n  "))
}

func (g *generator) pick(ids []int) int { return ids[g.rng.IntN(len(ids))] }

// schema returns the tables and their random starting rows.
func (g *generator) schema() string {
	var b strings.Builder
	b.WriteString(randomSchema)
	plans := []string{"pro", "trial", "basic"}
	customers := 6 + g.rng.IntN(7)
	for i := 1; i <= customers; i++ {
		note := "null"
		if g.rng.IntN(2) == 0 {
			note = fmt.Sprintf("'n%d'", i)
		}
		body := fmt.Sprintf("'b%d'", i)
		if g.rng.IntN(3) == 0 {
			body = fmt.Sprintf("repeat('x', %d)", 4000+i)
		}
		fmt.Fprintf(&b, "insert into customers values (%d, '%s', '555-%04d', %s, %d, '{\"k\": %d}', %s);\n",
			i, plans[g.rng.IntN(len(plans))], i, note, i*10, i, body)
	}
	orders := 8 + g.rng.IntN(13)
	for i := 1; i <= orders; i++ {
		fmt.Fprintf(&b, "insert into orders values (%d, %d, 'paid', %d.99);\n", i, 1+g.rng.IntN(customers), i*7%100)
	}
	g.agentCustomer, g.agentOrder, g.personCustomer, g.personOrder = 500, 5000, 900, 9000
	return b.String()
}

// value returns a new SQL literal for col. The agent's values and people's
// values never match each other, the starting data, or an earlier value.
func (g *generator) value(col string, person bool) string {
	g.n++
	n := g.n
	who := "a"
	if person {
		who = "p"
	}
	nullable := col != "plan" && col != "status" && col != "total"
	if !person && nullable && g.rng.IntN(10) == 0 {
		return "null"
	}
	switch col {
	case "score":
		if person {
			return strconv.Itoa(100000 + n)
		}
		return strconv.Itoa(1000 + n)
	case "tags":
		return fmt.Sprintf(`'{"%s": %d}'`, who, n)
	case "body":
		return fmt.Sprintf("repeat('%s', %d)", strings.ToUpper(who), 3000+n)
	case "total":
		if person {
			return fmt.Sprintf("%d.77", n)
		}
		return fmt.Sprintf("%d.11", n)
	default: // plan, phone, note, status
		return fmt.Sprintf("'%s%d'", who, n)
	}
}

// assignments returns "col = value, ..." for one to three distinct columns.
func (g *generator) assignments(columns []string, person bool) string {
	var chosen []string
	for range 1 + g.rng.IntN(3) {
		col := columns[g.rng.IntN(len(columns))]
		if !slices.Contains(chosen, col) {
			chosen = append(chosen, col)
		}
	}
	parts := make([]string, len(chosen))
	for i, col := range chosen {
		parts[i] = col + " = " + g.value(col, person)
	}
	return strings.Join(parts, ", ")
}

// state returns the ids of the customers and orders in a database, sorted, so
// the same seed makes the same choices.
func state(t *testing.T, conn *pgx.Conn) ([]int, []order) {
	t.Helper()
	var customers []int
	var orders []order
	for _, line := range column(t, conn, `select 'c ' || id from customers union all select 'o ' || id || ' ' || customer_id from orders`) {
		f := strings.Fields(line)
		id, _ := strconv.Atoi(f[1])
		if f[0] == "c" {
			customers = append(customers, id)
			continue
		}
		c, _ := strconv.Atoi(f[2])
		orders = append(orders, order{id, c})
	}
	slices.Sort(customers)
	slices.SortFunc(orders, func(a, b order) int { return a.id - b.id })
	return customers, orders
}

func orderIDs(orders []order) []int {
	ids := make([]int, len(orders))
	for i, o := range orders {
		ids[i] = o.id
	}
	return ids
}

// agentTransaction returns one to three random statements for the agent.
func (g *generator) agentTransaction(t *testing.T, rec *pgx.Conn) []string {
	customers, orders := state(t, rec)
	statements := make([]string, 1+g.rng.IntN(3))
	for i := range statements {
		statements[i] = g.agentStatement(customers, orders)
	}
	return statements
}

func (g *generator) agentStatement(customers []int, orders []order) string {
	markOrdersOf := func(customer int) {
		for _, o := range orders {
			if o.customer == customer {
				g.agentRows[ord(o.id)] = true
			}
		}
	}
	switch r := g.rng.IntN(100); {
	case r < 35 && len(customers) > 0:
		set := g.assignments(customerColumns, false)
		if g.rng.IntN(10) < 3 { // several rows at once
			lo := g.pick(customers)
			hi := lo + 1 + g.rng.IntN(3)
			for _, id := range customers {
				if id >= lo && id <= hi {
					g.agentRows[cust(id)] = true
				}
			}
			return fmt.Sprintf("update customers set %s where id between %d and %d", set, lo, hi)
		}
		id := g.pick(customers)
		g.agentRows[cust(id)] = true
		return fmt.Sprintf("update customers set %s where id = %d", set, id)
	case r < 55 && len(orders) > 0:
		id := g.pick(orderIDs(orders))
		g.agentRows[ord(id)] = true
		return fmt.Sprintf("update orders set %s where id = %d", g.assignments(orderColumns, false), id)
	case r < 70 && len(orders) > 0:
		o := orders[g.rng.IntN(len(orders))]
		if g.rng.IntN(4) == 0 { // all of one customer's orders
			markOrdersOf(o.customer)
			return fmt.Sprintf("delete from orders where customer_id = %d", o.customer)
		}
		g.agentRows[ord(o.id)] = true
		return fmt.Sprintf("delete from orders where id = %d", o.id)
	case r < 80 && len(customers) > 0: // cascades to the customer's orders
		id := g.pick(customers)
		g.agentRows[cust(id)] = true
		markOrdersOf(id)
		return fmt.Sprintf("delete from customers where id = %d", id)
	case r < 90 && len(customers) > 0:
		g.agentOrder++
		g.agentRows[ord(g.agentOrder)] = true
		return fmt.Sprintf("insert into orders values (%d, %d, %s, %s)",
			g.agentOrder, g.pick(customers), g.value("status", false), g.value("total", false))
	default:
		g.agentCustomer++
		g.agentRows[cust(g.agentCustomer)] = true
		return fmt.Sprintf("insert into customers values (%d, %s, %s, %s, %s, %s, %s)", g.agentCustomer,
			g.value("plan", false), g.value("phone", false), g.value("note", false),
			g.value("score", false), g.value("tags", false), g.value("body", false))
	}
}

// personStatement returns a random edit by a person, which is valid in both
// databases and has the same effect on every row they share.
func (g *generator) personStatement(t *testing.T, rec, ctl *pgx.Conn) string {
	recCustomers, recOrders := state(t, rec)
	ctlCustomers, ctlOrders := state(t, ctl)
	bothCustomers, agentCustomers := split(recCustomers, ctlCustomers)
	bothOrders, agentOrders := split(orderIDs(recOrders), orderIDs(ctlOrders))

	switch r := g.rng.IntN(100); {
	case r < 40 && len(bothCustomers)+len(agentCustomers) > 0:
		id := g.target(bothCustomers, agentCustomers, cust)
		g.touched[cust(id)] = true
		return fmt.Sprintf("update customers set %s where id = %d", g.assignments(customerColumns, true), id)
	case r < 55 && len(bothOrders)+len(agentOrders) > 0:
		id := g.target(bothOrders, agentOrders, ord)
		g.touched[ord(id)] = true
		// If the agent inserted this order's customer, undo can't delete the
		// customer without cascading to the person's edit, so a conflict on
		// the customer is explained too.
		for _, o := range recOrders {
			if o.id == id && slices.Contains(agentCustomers, o.customer) {
				g.touched[cust(o.customer)] = true
			}
		}
		return fmt.Sprintf("update orders set %s where id = %d", g.assignments(orderColumns, true), id)
	case r < 65 && len(bothOrders)+len(agentOrders) > 0:
		id := g.target(bothOrders, agentOrders, ord)
		g.touched[ord(id)] = true
		return fmt.Sprintf("delete from orders where id = %d", id)
	case r < 73 && len(bothCustomers)+len(agentCustomers) > 0:
		id := g.target(bothCustomers, agentCustomers, cust)
		g.touched[cust(id)] = true
		for _, o := range append(slices.Clone(recOrders), ctlOrders...) { // the cascade, in either database
			if o.customer == id {
				g.touched[ord(o.id)] = true
			}
		}
		return fmt.Sprintf("delete from customers where id = %d", id)
	case r < 88 && len(bothCustomers) > 0:
		g.personOrder++
		customer := g.pick(bothCustomers)
		g.touched[ord(g.personOrder)] = true
		return fmt.Sprintf("insert into orders values (%d, %d, %s, %s)",
			g.personOrder, customer, g.value("status", true), g.value("total", true))
	default:
		g.personCustomer++
		g.touched[cust(g.personCustomer)] = true
		return fmt.Sprintf("insert into customers values (%d, %s, %s, %s, %s, %s, %s)", g.personCustomer,
			g.value("plan", true), g.value("phone", true), g.value("note", true),
			g.value("score", true), g.value("tags", true), g.value("body", true))
	}
}

// target picks the row a person edits: usually one in both databases, and
// then usually one the agent targeted; sometimes a row only the agent
// inserted.
func (g *generator) target(both, agentOnly []int, name func(int) string) int {
	if len(agentOnly) > 0 && (len(both) == 0 || g.rng.IntN(100) < 15) {
		return g.pick(agentOnly)
	}
	var aimed []int
	for _, id := range both {
		if g.agentRows[name(id)] {
			aimed = append(aimed, id)
		}
	}
	if len(aimed) > 0 && g.rng.IntN(100) < 70 {
		return g.pick(aimed)
	}
	return g.pick(both)
}

// split returns the ids in both lists, and the ids only in recorded.
func split(recorded, control []int) (both, recordedOnly []int) {
	for _, id := range recorded {
		if slices.Contains(control, id) {
			both = append(both, id)
		} else {
			recordedOnly = append(recordedOnly, id)
		}
	}
	return both, recordedOnly
}

type randomStats struct {
	runs, agentTxns, agentFailed, agentStatements, peopleStatements, changes int
	refusedRuns, exactRuns, differing, reported, silent, unexplained         int
	actions                                                                  map[undo.Action]int
	examples                                                                 []string
}

func (s *randomStats) planRows() int {
	n := 0
	for _, v := range s.actions {
		n += v
	}
	return n
}

type randomTotals struct {
	mu sync.Mutex
	randomStats
}

func (tot *randomTotals) add(s *randomStats) {
	tot.mu.Lock()
	defer tot.mu.Unlock()
	if tot.actions == nil {
		tot.actions = map[undo.Action]int{}
	}
	tot.runs += s.runs
	tot.agentTxns += s.agentTxns
	tot.agentFailed += s.agentFailed
	tot.agentStatements += s.agentStatements
	tot.peopleStatements += s.peopleStatements
	tot.changes += s.changes
	tot.refusedRuns += s.refusedRuns
	tot.exactRuns += s.exactRuns
	tot.differing += s.differing
	tot.reported += s.reported
	tot.silent += s.silent
	tot.unexplained += s.unexplained
	for k, v := range s.actions {
		tot.actions[k] += v
	}
	tot.examples = append(tot.examples, s.examples...)
}

func envInt(t *testing.T, name string, def int) int {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		t.Fatalf("%s=%q: want a positive whole number", name, v)
	}
	return n
}
