package receipt

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// maxOpen bounds how many receipt files a Store keeps open at once.
const maxOpen = 64

// Store appends transactions to the receipts in one directory. Only one Store
// can use a directory at a time.
//
// Writing happens in two steps. Append and Commit write a transaction, and
// Sync makes everything committed so far durable. The recorder confirms a
// position to Postgres only after Sync returns.
type Store struct {
	dir     string
	lock    *os.File
	runs    map[string]*runFile // every run seen this session
	open    int                 // how many runs have an open file
	newFile bool                // the directory needs syncing
}

// runFile is what the Store knows about one run's receipt.
type runFile struct {
	committed int64 // bytes up to the end of the last commit line
	lastEnd   LSN   // end LSN of the last commit
	size      int64 // bytes written, including any still in w
	repaired  bool  // a cut-off tail has been removed this session
	dirty     bool  // committed data that may not be on disk yet
	f         *os.File
	w         *bufio.Writer // nil while the file is closed
}

// Open opens the receipts directory, creating it if needed, and locks it.
// The directory must not be readable by other users: receipts hold whole
// rows, which can include personal data.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("receipts path %s is not a directory", dir)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("receipts directory %s can be read by other users; run chmod 700 %s", dir, dir)
	}
	lock, err := lockDir(dir)
	if err != nil {
		return nil, err
	}
	return &Store{dir: dir, lock: lock, runs: map[string]*runFile{}}, nil
}

// LastEnd returns the end LSN of the run's last committed transaction, or 0
// if the run has none. The recorder uses it to skip transactions that
// Postgres sends again after a restart. It creates nothing.
func (s *Store) LastEnd(run string) (LSN, error) {
	r, err := s.run(run)
	if err != nil {
		return 0, err
	}
	return r.lastEnd, nil
}

// Append adds a change to the run's current transaction.
func (s *Store) Append(run string, c *Change) error {
	r, err := s.writer(run)
	if err != nil {
		return err
	}
	return r.write(line{Kind: kindChange, Change: c})
}

// Commit ends the run's current transaction. It reaches the disk at the next
// Sync.
func (s *Store) Commit(run string, c *Commit) error {
	r, err := s.writer(run)
	if err != nil {
		return err
	}
	if err := r.write(line{Kind: kindCommit, Commit: c}); err != nil {
		return err
	}
	if err := r.w.Flush(); err != nil {
		return fmt.Errorf("write receipt %s: %w", run, err)
	}
	r.committed, r.lastEnd, r.dirty = r.size, c.EndLSN, true
	return nil
}

// Discard drops every change that has no commit line yet, for example when
// the connection was lost in the middle of a transaction.
func (s *Store) Discard() error {
	for run, r := range s.runs {
		if r.size == r.committed {
			continue
		}
		if r.w != nil {
			r.w.Reset(r.f)
		}
		if err := os.Truncate(path(s.dir, run), r.committed); err != nil {
			return fmt.Errorf("discard unfinished transaction in %s: %w", run, err)
		}
		r.size = r.committed
	}
	return nil
}

// Sync makes every committed transaction durable, including ones a previous
// session wrote but may not have synced.
func (s *Store) Sync() error {
	for run, r := range s.runs {
		if !r.dirty {
			continue
		}
		if err := syncFile(r.f, path(s.dir, run)); err != nil {
			return fmt.Errorf("sync receipt %s: %w", run, err)
		}
		r.dirty = false
	}
	if s.newFile {
		// A file's name is durable only once its directory is synced.
		if err := syncFile(nil, s.dir); err != nil {
			return err
		}
		s.newFile = false
	}
	return nil
}

// Close drops any unfinished transaction, closes the files and releases the
// directory lock. It does not sync.
func (s *Store) Close() error {
	err := errors.Join(s.Discard(), s.closeFiles())
	s.runs = nil
	return errors.Join(err, s.lock.Close())
}

// run returns what the Store knows about a run, reading its receipt (without
// creating it) the first time.
func (s *Store) run(run string) (*runFile, error) {
	if r, ok := s.runs[run]; ok {
		return r, nil
	}
	if !ValidRunID(run) {
		return nil, fmt.Errorf("invalid run ID %q", run)
	}
	r := &runFile{}
	f, err := os.Open(path(s.dir, run))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		r.committed, r.lastEnd, err = scan(f, nil)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("open receipt %s: %w", run, err)
		}
		// A previous session may have written these bytes without syncing
		// them. They must be on disk before anything relies on them.
		r.size, r.dirty, s.newFile = r.committed, r.committed > 0, s.newFile || r.committed > 0
	}
	s.runs[run] = r
	return r, nil
}

// writer returns the run's receipt open for appending. The first time in a
// session it also cuts off a transaction left unfinished at the end.
func (s *Store) writer(run string) (*runFile, error) {
	r, err := s.run(run)
	if err != nil || r.w != nil {
		return r, err
	}
	if s.open >= maxOpen {
		if err := s.closeFiles(); err != nil {
			return nil, err
		}
	}
	p := path(s.dir, run)
	_, statErr := os.Lstat(p)
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	if !r.repaired {
		if err := f.Truncate(r.committed); err != nil {
			f.Close()
			return nil, fmt.Errorf("repair receipt %s: %w", run, err)
		}
		r.repaired, r.size = true, r.committed
	}
	r.f, r.w = f, bufio.NewWriter(f)
	s.open++
	s.newFile = s.newFile || errors.Is(statErr, os.ErrNotExist)
	return r, nil
}

// closeFiles flushes and closes every open receipt. They reopen on demand.
func (s *Store) closeFiles() error {
	var err error
	for _, r := range s.runs {
		if r.w == nil {
			continue
		}
		err = errors.Join(err, r.w.Flush(), r.f.Close())
		r.f, r.w = nil, nil
	}
	s.open = 0
	return err
}

func (r *runFile) write(l line) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // keep <, > and & readable
	if err := enc.Encode(l); err != nil {
		return err
	}
	n, err := r.w.Write(buf.Bytes())
	r.size += int64(n)
	return err
}

// syncFile fsyncs f, or the file or directory at path when f is nil.
func syncFile(f *os.File, path string) error {
	if f != nil {
		return f.Sync()
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
