package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/pu-suo/agent-undo/internal/capture"
	"github.com/pu-suo/agent-undo/internal/receipt"
)

func runRecord(ctx context.Context, a *app, args []string) error {
	fs := a.flags("record")
	dbURL, receipts := dbFlag(fs), receiptsFlag(fs)
	if _, err := parse(fs, args, 0); err != nil {
		return err
	}
	cfg, err := a.config(*dbURL)
	if err != nil {
		return err
	}
	rec := &capture.Recorder{Config: cfg, Receipts: *receipts, Log: a.log}
	return rec.Record(ctx)
}

func runShow(_ context.Context, a *app, args []string) error {
	fs := a.flags("show")
	receipts := receiptsFlag(fs)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return listRuns(a.stdout, *receipts)
	}
	if fs.NArg() > 1 {
		return usageError{"want at most one run ID (flags go before it)"}
	}
	run := fs.Arg(0)
	txns, err := receipt.Read(*receipts, run)
	if err != nil {
		return err
	}
	changes := 0
	for _, t := range txns {
		changes += len(t.Changes)
	}
	fmt.Fprintf(a.stdout, "%s: %d transaction(s), %d change(s)\n", run, len(txns), changes)
	for _, t := range txns {
		printTransaction(a.stdout, t)
	}
	return nil
}

func listRuns(w io.Writer, dir string) error {
	runs, err := receipt.Runs(dir)
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		fmt.Fprintf(w, "No receipts in %s yet.\n", dir)
	}
	for _, r := range runs {
		fmt.Fprintln(w, r)
	}
	return nil
}

func printTransaction(w io.Writer, t receipt.Transaction) {
	c := t.Commit
	fmt.Fprintf(w, "\ntransaction %d, committed %s at %s\n", c.XID, c.Time.Format(time.DateTime+" MST"), c.CommitLSN)
	if c.Incomplete != "" {
		fmt.Fprintln(w, printable("  INCOMPLETE, so undo will refuse this run: "+c.Incomplete))
	}
	for _, ch := range t.Changes {
		fmt.Fprintln(w, printable(fmt.Sprintf("  %-8s %s.%s  %s", ch.Op, ch.Schema, ch.Table, describe(ch))))
	}
}

// describe summarizes a change: for an update, the changed columns and then
// the rest of the row; otherwise the whole row.
func describe(c receipt.Change) string {
	switch c.Op {
	case receipt.Truncate:
		return "(cannot be undone)"
	case receipt.Insert:
		return "(" + values(c.New, c.New.Columns()) + ")"
	case receipt.Delete:
		return "(" + values(c.Old, c.Old.Columns()) + ")"
	}
	var changed, same []string
	for _, col := range c.New.Columns() {
		if receipt.Same(c.Old[col], c.New[col]) {
			same = append(same, col)
			continue
		}
		changed = append(changed, col+": "+receipt.Quote(c.Old[col])+" → "+receipt.Quote(c.New[col]))
	}
	return strings.Join(changed, ", ") + "  (" + values(c.New, same) + ")"
}

// values prints col=value pairs for the given columns, in that order.
func values(row receipt.Row, cols []string) string {
	parts := make([]string, len(cols))
	for i, col := range cols {
		parts[i] = col + "=" + receipt.Quote(row[col])
	}
	return strings.Join(parts, " ")
}
