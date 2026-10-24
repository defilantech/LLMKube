package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// engineRecordVersion is the record format. A record with any other version is
// ignored by adoption (PR 2) rather than misread.
const engineRecordVersion = 2

const (
	engineRecordProvisional = "provisional"
	engineRecordRunning     = "running"
)

// SpawnedProcess is what an executor knows about a child right after
// cmd.Start, passed to ExecutorConfig.OnSpawned.
type SpawnedProcess struct {
	PID  int
	PGID int
	Port int
	// Executable is the spawned program with symlinks resolved; LaunchPath is
	// the path as executed. A Homebrew relink changes what the first resolves
	// to, so adoption accepts either.
	Executable string
	LaunchPath string
	ModelPath  string
	LogPath    string
	// BindHost is the address the engine listens on: loopback in relay
	// mode, all interfaces in legacy mode.
	BindHost string
}

// engineRecord is the on-disk record of one engine the agent started, read by
// a restarted agent to adopt the engine instead of reloading it (#2079).
type engineRecord struct {
	Version            int       `json:"version"`
	AgentVersion       string    `json:"agentVersion,omitempty"`
	Namespace          string    `json:"namespace"`
	Name               string    `json:"name"`
	State              string    `json:"state"`
	PID                int       `json:"pid"`
	PGID               int       `json:"pgid"`
	StartToken         string    `json:"startToken,omitempty"`
	Executable         string    `json:"executable,omitempty"`
	LaunchPath         string    `json:"launchPath,omitempty"`
	BindHost           string    `json:"bindHost,omitempty"`
	Port               int       `json:"port"`
	Runtime            string    `json:"runtime"`
	ModelPath          string    `json:"modelPath,omitempty"`
	ModelID            string    `json:"modelID,omitempty"`
	LogPath            string    `json:"logPath,omitempty"`
	SpecHash           string    `json:"specHash,omitempty"`
	Priority           string    `json:"priority,omitempty"`
	EvictionProtection bool      `json:"evictionProtection,omitempty"`
	StartedAt          time.Time `json:"startedAt,omitzero"`
	// AdmittedBytes is the memory estimate the engine was admitted with, so an
	// agent that adopts it counts that memory as committed (#2080).
	AdmittedBytes uint64 `json:"admittedBytes,omitempty"`
}

// executablePaths are the paths the engine was recorded under, resolved and
// as executed, for executableMatches.
func (r engineRecord) executablePaths() []string {
	return []string{r.Executable, r.LaunchPath}
}

// engineRecordStore keeps one record file per engine under dir
// (<stateDir>/engines). Writes are atomic and private (0700 dir, 0600 files).
// Callers serialize writes per service (the agent's starting set does), so the
// store needs no lock of its own: different services touch different files.
type engineRecordStore struct {
	dir          string
	dirErr       error // non-nil when the state directory could not be resolved
	agentVersion string
	inspect      processInspector
}

func newEngineRecordStore(dir, agentVersion string, inspect processInspector, dirErr error) *engineRecordStore {
	return &engineRecordStore{dir: dir, dirErr: dirErr, agentVersion: agentVersion, inspect: inspect}
}

// Dir is the records directory, for messages.
func (s *engineRecordStore) Dir() string { return s.dir }

func (s *engineRecordStore) path(namespace, name string) string {
	return filepath.Join(s.dir, namespace+"_"+name+".json")
}

// WriteProvisional records a child the moment it is spawned, so an agent that
// dies during a long model load still leaves a record of it.
func (s *engineRecordStore) WriteProvisional(namespace, name, runtime string, sp SpawnedProcess) error {
	tok, err := s.startToken(sp.PID)
	if err != nil {
		return err
	}
	return s.write(engineRecord{
		Namespace: namespace, Name: name, State: engineRecordProvisional, PID: sp.PID, PGID: sp.PGID,
		StartToken: tok, Executable: sp.Executable, LaunchPath: sp.LaunchPath, Port: sp.Port, Runtime: runtime,
		ModelPath: sp.ModelPath, LogPath: sp.LogPath, BindHost: sp.BindHost,
	})
}

// WriteRunning completes (or, for a shared-daemon service with no spawn,
// creates) the record of a stored process, keeping the executable paths,
// process group, log path and bind host a provisional record captured. On success it sets
// p.StartToken and p.PGID to the values written, so the process carries the
// identity its record holds.
func (s *engineRecordStore) WriteRunning(p *ManagedProcess) error {
	tok, err := s.startToken(p.PID)
	if err != nil {
		return err
	}
	rec := engineRecord{
		Namespace: p.Namespace, Name: p.Name, State: engineRecordRunning, PID: p.PID, StartToken: tok,
		Port: p.Port, Runtime: p.Runtime, ModelPath: p.ModelPath, ModelID: p.ModelID, SpecHash: p.SpecHash,
		Priority: p.Priority, EvictionProtection: p.EvictionProtection, StartedAt: p.StartedAt,
		AdmittedBytes: p.AdmittedBytes,
	}
	if prev, ok := s.read(p.Namespace, p.Name); ok && prev.PID == p.PID {
		rec.Executable, rec.LaunchPath = prev.Executable, prev.LaunchPath
		rec.LogPath, rec.PGID, rec.BindHost = prev.LogPath, prev.PGID, prev.BindHost
	}
	if err := s.write(rec); err != nil {
		return err
	}
	p.StartToken, p.PGID = rec.StartToken, rec.PGID
	return nil
}

// List reads every record in the directory. Records of another version or
// unparsable files are skipped and reported, never acted on.
func (s *engineRecordStore) List() ([]engineRecord, []error) {
	if s.dirErr != nil {
		return nil, []error{s.dirErr}
	}
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []error{fmt.Errorf("list engine records in %s: %w", s.dir, err)}
	}
	var recs []engineRecord
	var errs []error
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".json" || strings.HasPrefix(name, ".") {
			continue
		}
		ns, n, ok := strings.Cut(strings.TrimSuffix(name, ".json"), "_")
		if !ok {
			errs = append(errs, fmt.Errorf("engine record %s: unexpected file name", name))
			continue
		}
		rec, ok := s.read(ns, n)
		if !ok {
			errs = append(errs, fmt.Errorf("engine record %s: unreadable or not version %d", name, engineRecordVersion))
			continue
		}
		recs = append(recs, rec)
	}
	return recs, errs
}

// Remove deletes the record; a missing record is not an error.
func (s *engineRecordStore) Remove(namespace, name string) error {
	if s.dirErr != nil {
		return nil
	}
	if err := os.Remove(s.path(namespace, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove engine record %s: %w", s.path(namespace, name), err)
	}
	return nil
}

// startToken is empty for PID 0 (a daemon the agent does not own, e.g. Ollama).
func (s *engineRecordStore) startToken(pid int) (string, error) {
	if pid <= 0 {
		return "", nil
	}
	tok, err := s.inspect.StartToken(pid)
	if err != nil {
		return "", fmt.Errorf("engine record: start time of PID %d: %w", pid, err)
	}
	return tok, nil
}

func (s *engineRecordStore) read(namespace, name string) (engineRecord, bool) {
	// O_NOFOLLOW: a symlink planted in the records directory is no record.
	f, err := os.OpenFile(s.path(namespace, name), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return engineRecord{}, false
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(f)
	if err != nil {
		return engineRecord{}, false
	}
	var rec engineRecord
	if json.Unmarshal(data, &rec) != nil || rec.Version != engineRecordVersion {
		return engineRecord{}, false
	}
	return rec, true
}

func (s *engineRecordStore) write(rec engineRecord) error {
	if s.dirErr != nil {
		return fmt.Errorf("engine records directory unavailable: %w", s.dirErr)
	}
	rec.Version = engineRecordVersion
	rec.AgentVersion = s.agentVersion
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create engine records directory %s: %w", s.dir, err)
	}
	if err := os.Chmod(s.dir, 0o700); err != nil { //nolint:gosec // a directory needs the owner search bit
		return fmt.Errorf("restrict engine records directory %s: %w", s.dir, err)
	}
	tmp, err := os.CreateTemp(s.dir, ".record-*")
	if err != nil {
		return fmt.Errorf("write engine record in %s: %w", s.dir, err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write engine record %s: %w", tmp.Name(), err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync engine record %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	dst := s.path(rec.Namespace, rec.Name)
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return fmt.Errorf("write engine record %s: %w", dst, err)
	}
	ok = true
	return nil
}
