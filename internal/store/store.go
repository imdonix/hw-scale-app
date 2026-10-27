// Package store persists the daemon/pairing state (state.json) and the
// single-instance lock in the data directory. Measurements live in SQLite
// (package db).
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/tamasmagyar/sscale/internal/scale"
)

const (
	stateFile = "state.json"
	pidFile   = "sscale.pid"
	lockFile  = "sscale.lock"
)

// Session describes the outcome of one connection.
type Session struct {
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
	Result  string    `json:"result"`
	Error   string    `json:"error,omitempty"`
	Records int       `json:"records"`
}

// State is the persisted daemon state (state.json).
type State struct {
	Daemon struct {
		PID       int       `json:"pid,omitempty"`
		StartedAt time.Time `json:"started_at,omitempty"`
		Heartbeat time.Time `json:"heartbeat,omitempty"`
		Phase     string    `json:"phase,omitempty"`
		Adapter   string    `json:"adapter,omitempty"`
	} `json:"daemon"`
	Scale struct {
		Address  string     `json:"address"`
		Name     string     `json:"name,omitempty"`
		RSSI     int        `json:"rssi,omitempty"`
		LastSeen *time.Time `json:"last_seen,omitempty"`
		Awake    bool       `json:"awake"`
	} `json:"scale"`
	Pairing struct {
		Paired     bool             `json:"paired"`
		PairedAt   *time.Time       `json:"paired_at,omitempty"`
		HUIDSource string           `json:"huid_source,omitempty"`
		Device     scale.DeviceInfo `json:"device"`
	} `json:"pairing"`
	LastSession *Session   `json:"last_session,omitempty"`
	LastSuccess *time.Time `json:"last_success,omitempty"`
	Sessions    int        `json:"sessions"`
}

// Store is the data directory.
type Store struct {
	Dir string
	mu  sync.Mutex
	st  State
}

// Open loads the store, creating the directory if needed.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	s := &Store{Dir: dir}
	b, err := os.ReadFile(filepath.Join(dir, stateFile))
	if err == nil {
		if err := json.Unmarshal(b, &s.st); err != nil {
			return nil, fmt.Errorf("parse %s: %w", stateFile, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return s, nil
}

// LoadState reads state.json without opening a Store for writing.
func LoadState(dir string) (State, error) {
	var st State
	b, err := os.ReadFile(filepath.Join(dir, stateFile))
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(b, &st)
}

// Update mutates the state under lock and saves it.
func (s *Store) Update(fn func(*State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.st)
	return s.saveLocked()
}

// Snapshot returns a copy of the state.
func (s *Store) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.st
	return st
}

func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(&s.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.Dir, stateFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.Dir, stateFile))
}

// ---------------------------------------------------------- lock & pid

// Lock is the single-instance daemon lock.
type Lock struct{ f *os.File }

// ErrLocked means another sscale process holds the lock.
var ErrLocked = errors.New("another sscale daemon/session is running")

// AcquireLock takes the exclusive lock (non-blocking).
func AcquireLock(dir string) (*Lock, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, lockFile), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return &Lock{f: f}, nil
}

// Release releases the lock.
func (l *Lock) Release() {
	if l != nil && l.f != nil {
		_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
		l.f.Close()
	}
}

// IsLocked reports whether some process holds the lock.
func IsLocked(dir string) bool {
	l, err := AcquireLock(dir)
	if err != nil {
		return errors.Is(err, ErrLocked)
	}
	l.Release()
	return false
}

// WritePID writes the daemon pid file.
func WritePID(dir string) error {
	return os.WriteFile(filepath.Join(dir, pidFile), []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644)
}

// RemovePID removes the pid file.
func RemovePID(dir string) { _ = os.Remove(filepath.Join(dir, pidFile)) }

// ReadPID reads the daemon pid.
func ReadPID(dir string) (int, error) {
	b, err := os.ReadFile(filepath.Join(dir, pidFile))
	if err != nil {
		return 0, err
	}
	var pid int
	_, err = fmt.Sscanf(string(b), "%d", &pid)
	return pid, err
}
