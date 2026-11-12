package receipt

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Read returns the committed transactions in a run's receipt, oldest first.
func Read(dir, run string) ([]Transaction, error) {
	if !ValidRunID(run) {
		return nil, fmt.Errorf("invalid run ID %q", run)
	}
	f, err := os.Open(path(dir, run))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", run, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var txns []Transaction
	var pending []Change
	_, _, err = scan(f, func(l line) {
		if l.Kind == kindChange {
			pending = append(pending, *l.Change)
			return
		}
		txns = append(txns, Transaction{Commit: *l.Commit, Changes: pending})
		pending = nil
	})
	if err != nil {
		return nil, fmt.Errorf("read receipt %s: %w", run, err)
	}
	return txns, nil
}

// Runs lists the runs that have a receipt in dir, sorted by name.
func Runs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var runs []string
	for _, e := range entries {
		if run, ok := strings.CutSuffix(e.Name(), ".jsonl"); ok && ValidRunID(run) {
			runs = append(runs, run)
		}
	}
	slices.Sort(runs)
	return runs, nil
}

// scan reads receipt lines and calls fn for each line of a committed
// transaction. It returns the length of the committed part of the file and
// the end LSN of its last commit. A cut-off final line or an unfinished
// transaction at the end is not an error: it is simply not committed. With
// fn nil, it only looks at each line's kind, which is much cheaper.
func scan(r io.Reader, fn func(line)) (committed int64, lastEnd LSN, err error) {
	br := bufio.NewReader(r)
	parse := parseLine
	if fn == nil {
		parse = parseHead
	}
	var offset int64
	var pending []line // lines of the transaction being read
	var corrupt error  // first unreadable line after the last commit
	for n := 1; ; n++ {
		raw, err := br.ReadBytes('\n')
		if err == io.EOF {
			return committed, lastEnd, nil // a final line without '\n' was cut off
		}
		if err != nil {
			return 0, 0, err
		}
		offset += int64(len(raw))

		l, err := parse(raw)
		if err != nil {
			if corrupt == nil {
				corrupt = fmt.Errorf("line %d: %w", n, err)
			}
			continue
		}
		if l.Kind == kindChange {
			if fn != nil {
				pending = append(pending, l)
			}
			continue
		}
		if corrupt != nil {
			// An unreadable line inside a committed transaction means damage,
			// not a cut-off write.
			return 0, 0, corrupt
		}
		if fn != nil {
			for _, p := range pending {
				fn(p)
			}
			fn(l)
		}
		pending = pending[:0]
		committed, lastEnd = offset, l.Commit.EndLSN
	}
}

// parseHead reads only a line's kind and, for a commit, its end LSN.
func parseHead(raw []byte) (line, error) {
	var h struct {
		Kind   string `json:"kind"`
		EndLSN LSN    `json:"end_lsn"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return line{}, err
	}
	switch h.Kind {
	case kindChange:
		return line{Kind: kindChange}, nil
	case kindCommit:
		return line{Kind: kindCommit, Commit: &Commit{EndLSN: h.EndLSN}}, nil
	}
	return line{}, fmt.Errorf("unexpected line kind %q", h.Kind)
}

func parseLine(raw []byte) (line, error) {
	var l line
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&l); err != nil {
		return line{}, err
	}
	switch {
	case l.Kind == kindChange && l.Change != nil && l.Commit == nil:
	case l.Kind == kindCommit && l.Commit != nil && l.Change == nil:
	default:
		return line{}, fmt.Errorf("unexpected line kind %q", l.Kind)
	}
	return l, nil
}

func path(dir, run string) string {
	return filepath.Join(dir, run+".jsonl")
}
