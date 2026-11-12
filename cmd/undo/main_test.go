package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pu-suo/agent-undo/internal/receipt"
)

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, &stdout, &stderr, func(string) string { return "" })
	return code, stdout.String(), stderr.String()
}

func TestUsageAndExitCodes(t *testing.T) {
	tests := []struct {
		args []string
		code int
		want string // in stdout or stderr
	}{
		{nil, 0, "Commands:"},
		{[]string{"help"}, 0, "preview"},
		{[]string{"nope"}, 2, `unknown command "nope"`},
		{[]string{"show", "-bogus"}, 2, "flag provided but not defined"},
		{[]string{"preview"}, 2, "want 1 argument(s), got 0"},
		{[]string{"apply", "run-1"}, 2, "--plan is required"},
		{[]string{"record", "-h"}, 0, "-receipts"},
	}
	for _, tt := range tests {
		code, stdout, stderr := runCLI(t, tt.args...)
		if code != tt.code || !strings.Contains(stdout+stderr, tt.want) {
			t.Errorf("undo %v = exit %d, output %q; want exit %d and %q", tt.args, code, stdout+stderr, tt.code, tt.want)
		}
	}
}

func TestShow(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "receipts")
	store, err := receipt.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	str := func(s string) *string { return &s }
	err = store.Append("run-42", &receipt.Change{Op: receipt.Update, RelID: 1, Schema: "public", Table: "customers",
		Old: receipt.Row{"id": str("1"), "plan": str("pro")}, New: receipt.Row{"id": str("1"), "plan": str("cancelled\x1b[31m")}})
	if err == nil {
		err = store.Commit("run-42", &receipt.Commit{XID: 770, CommitLSN: 0x100, EndLSN: 0x130, Time: time.Unix(0, 0), Changes: 1})
	}
	if err != nil || store.Close() != nil {
		t.Fatal(err)
	}

	code, stdout, _ := runCLI(t, "show", "-receipts", dir, "run-42")
	if code != 0 || !strings.Contains(stdout, `plan: "pro" → "cancelled\x1b[31m"`) {
		t.Errorf("show = exit %d:\n%s", code, stdout)
	}
	if code, stdout, _ := runCLI(t, "show", "-receipts", dir); code != 0 || strings.TrimSpace(stdout) != "run-42" {
		t.Errorf("show with no run = exit %d, %q; want the list of runs", code, stdout)
	}
	if code, _, stderr := runCLI(t, "show", "-receipts", dir, "run-43"); code != 1 || !strings.Contains(stderr, "no receipt") {
		t.Errorf("show of a missing run = exit %d, %q", code, stderr)
	}
}
