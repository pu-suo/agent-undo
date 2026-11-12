package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/pu-suo/agent-undo/internal/receipt"
	"github.com/pu-suo/agent-undo/internal/undo"
)

func runPreview(ctx context.Context, a *app, args []string) error {
	fs := a.flags("preview")
	dbURL, receipts := dbFlag(fs), receiptsFlag(fs)
	asJSON := fs.Bool("json", false, "print the plan as JSON")
	rest, err := parse(fs, args, 1)
	if err != nil {
		return err
	}
	conn, _, err := a.connect(ctx, *dbURL)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())

	plan, err := undo.Preview(ctx, conn, undo.Options{Receipts: *receipts, Run: rest[0]})
	if err != nil {
		return err
	}
	if err := printPlan(a.stdout, plan, *asJSON); err != nil {
		return err
	}
	if plan.Refused == "" && !*asJSON {
		fmt.Fprintf(a.stdout, "\nNothing has changed yet. To apply this plan: undo apply --plan %s %s\n", plan.Hash, plan.Run)
	}
	if plan.NeedsPerson() {
		return errNeedsPerson
	}
	return nil
}

func runApply(ctx context.Context, a *app, args []string) error {
	fs := a.flags("apply")
	dbURL, receipts := dbFlag(fs), receiptsFlag(fs)
	hash := fs.String("plan", "", "the plan hash printed by undo preview (required)")
	wait := fs.Duration("wait", 30*time.Second, "how long to wait for the run's open transactions and the recorder")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	rest, err := parse(fs, args, 1)
	if err != nil {
		return err
	}
	if *hash == "" {
		return usageError{"--plan is required: run undo preview first and pass the hash it prints"}
	}
	conn, _, err := a.connect(ctx, *dbURL)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())

	plan, err := undo.Apply(ctx, conn, undo.Options{Receipts: *receipts, Run: rest[0], Wait: *wait, PlanHash: *hash})
	if err != nil {
		return err
	}
	if err := printPlan(a.stdout, plan, *asJSON); err != nil {
		return err
	}
	if plan.Applied && !*asJSON {
		fmt.Fprintf(a.stdout, "\nApplied. The database now refuses writes from %s.\n", plan.Run)
	}
	if plan.NeedsPerson() {
		return errNeedsPerson
	}
	return nil
}

// printPlan prints one line per row, in the order the run changed them, then
// a summary.
func printPlan(w io.Writer, plan *undo.Plan, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(plan)
	}
	heading := "Undo plan"
	if plan.Applied {
		heading = "Undone"
	}
	fmt.Fprintf(w, "%s for %s (plan %s)\n", heading, plan.Run, plan.Hash)
	for _, warning := range plan.Warnings {
		fmt.Fprintln(w, "  warning: "+warning)
	}
	if plan.Refused != "" {
		fmt.Fprintln(w, printable("  REFUSED: "+plan.Refused))
		fmt.Fprintln(w, "  Nothing will be changed.")
		return nil
	}
	if len(plan.Rows) == 0 {
		fmt.Fprintln(w, "  The run changed nothing.")
	}
	counts := map[undo.Action]int{}
	for _, r := range plan.Rows {
		counts[r.Action]++
		line := fmt.Sprintf("  %-8s %s %s", r.Action, r.Table, r.Key)
		if detail := rowDetail(r); detail != "" {
			line += "  " + detail
		}
		fmt.Fprintln(w, printable(line))
		for _, f := range r.Flags {
			fmt.Fprintln(w, printable("           kept: "+f))
		}
		for _, n := range r.Notes {
			fmt.Fprintln(w, printable("           note: "+n))
		}
	}
	var parts []string
	for _, act := range []undo.Action{undo.Restore, undo.Partial, undo.Keep, undo.Nothing, undo.Refuse} {
		if counts[act] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[act], act))
		}
	}
	if len(parts) > 0 {
		fmt.Fprintln(w, "Rows: "+strings.Join(parts, ", "))
	}
	return nil
}

func rowDetail(r undo.RowPlan) string {
	switch {
	case r.Reason != "":
		return r.Reason
	case r.Write == undo.DeleteRow:
		return "delete the row the agent inserted"
	case r.Write == undo.InsertRow:
		return "insert the deleted row (" + values(r.Values, r.Columns) + ")"
	case r.Write == undo.UpdateRow:
		set := make([]string, len(r.Columns))
		for i, col := range r.Columns {
			set[i] = col + " = " + receipt.Quote(r.Values[col])
		}
		return "set " + strings.Join(set, ", ")
	}
	return ""
}
