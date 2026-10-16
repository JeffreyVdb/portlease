//go:build linux

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type lease struct {
	Result    result    `json:"reservation"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	OwnerPID  int       `json:"owner_pid,omitempty"`
	OwnerBoot string    `json:"owner_start_time,omitempty"`
}

type state struct {
	Schema string  `json:"schema"`
	Leases []lease `json:"leases"`
}

type store struct {
	dir string
}

func newStore(path string) (*store, error) {
	if path == "" {
		base := os.Getenv("XDG_RUNTIME_DIR")
		if base == "" {
			base = os.TempDir()
		}
		path = filepath.Join(base, "portlease-"+strconv.Itoa(os.Getuid()))
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect state directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("state path is not a real directory")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Getuid() {
		return nil, errors.New("state directory is not owned by the current user")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("state directory must not be accessible by group or other users")
	}
	return &store{dir: path}, nil
}

func (s *store) update(fn func(*state) error) error {
	lockPath := filepath.Join(s.dir, "lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open state lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock state: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	st, err := s.read()
	if err != nil {
		return err
	}
	prune(&st)
	if err := fn(&st); err != nil {
		return err
	}
	return s.write(st)
}

func (s *store) read() (state, error) {
	path := filepath.Join(s.dir, "state.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return state{Schema: schema, Leases: []lease{}}, nil
	}
	if err != nil {
		return state{}, fmt.Errorf("read state: %w", err)
	}
	var st state
	if err := json.Unmarshal(data, &st); err != nil {
		return state{}, fmt.Errorf("parse state (refusing to discard it): %w", err)
	}
	if st.Schema != schema {
		return state{}, fmt.Errorf("unsupported state schema %q", st.Schema)
	}
	return st, nil
}

func (s *store) write(st state) error {
	temp, err := os.CreateTemp(s.dir, ".state-*")
	if err != nil {
		return fmt.Errorf("create state temporary file: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	encoder := json.NewEncoder(temp)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(st); err != nil {
		temp.Close()
		return fmt.Errorf("encode state: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("sync state: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close state: %w", err)
	}
	if err := os.Rename(tempName, filepath.Join(s.dir, "state.json")); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	return nil
}

func prune(st *state) {
	now := time.Now()
	kept := st.Leases[:0]
	for _, item := range st.Leases {
		if !item.ExpiresAt.IsZero() && !now.Before(item.ExpiresAt) {
			continue
		}
		if item.OwnerPID != 0 && processStart(item.OwnerPID) != item.OwnerBoot {
			continue
		}
		kept = append(kept, item)
	}
	st.Leases = kept
}

func processStart(pid int) string {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return ""
	}
	// comm is parenthesized and may contain spaces or ')'; fields after the
	// final ')' begin at field 3. starttime is field 22.
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return ""
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) <= 19 {
		return ""
	}
	return fields[19]
}
