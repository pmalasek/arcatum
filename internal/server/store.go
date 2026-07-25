package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Store is the in-memory state: instances (loaded from JSON) and runs. Run output
// is written to files under backupDir/runs/<runID>/ — kept on the server, not on
// the backed-up host. A DB replaces this later.
type Store struct {
	mu        sync.Mutex
	instances map[string]*Instance
	runs      map[string]*Run
	runOrder  []string
	backupDir string
	seq       int64
}

// NewStore creates an empty store writing run data under backupDir.
func NewStore(backupDir string) *Store {
	return &Store{
		instances: map[string]*Instance{},
		runs:      map[string]*Run{},
		backupDir: backupDir,
	}
}

// LoadInstances reads instances from a JSON array file. A missing file is fine.
func (s *Store) LoadInstances(path string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var list []*Instance
	if err := json.Unmarshal(data, &list); err != nil {
		return fmt.Errorf("parse instances %s: %w", path, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, in := range list {
		if in.ID == "" {
			return fmt.Errorf("instances %s: an instance is missing id", path)
		}
		s.instances[in.ID] = in
	}
	return nil
}

// Instances returns all instances (order unspecified).
func (s *Store) Instances() []*Instance {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Instance, 0, len(s.instances))
	for _, in := range s.instances {
		out = append(out, in)
	}
	return out
}

// InstancesForRunner returns instances targeting a runner.
func (s *Store) InstancesForRunner(runnerID string) []*Instance {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Instance
	for _, in := range s.instances {
		if in.RunnerID == runnerID {
			out = append(out, in)
		}
	}
	return out
}

// Instance returns one instance by id.
func (s *Store) Instance(id string) (*Instance, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.instances[id]
	return in, ok
}

// CreateRun records a new pending run and returns it.
func (s *Store) CreateRun(inst *Instance) *Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	r := &Run{
		ID:         fmt.Sprintf("run-%d", s.seq),
		InstanceID: inst.ID,
		RunnerID:   inst.RunnerID,
		Script:     inst.Script,
		Status:     StatusPending,
	}
	s.runs[r.ID] = r
	s.runOrder = append(s.runOrder, r.ID)
	return r
}

// Run returns a run by id.
func (s *Store) Run(id string) (*Run, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	return r, ok
}

// UpdateRun applies fn to a run under the store lock.
func (s *Store) UpdateRun(id string, fn func(*Run)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok {
		return false
	}
	fn(r)
	return true
}

// ListRuns returns runs newest-first.
func (s *Store) ListRuns() []*Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Run, 0, len(s.runOrder))
	for i := len(s.runOrder) - 1; i >= 0; i-- {
		out = append(out, s.runs[s.runOrder[i]])
	}
	return out
}

// AppendOutput appends streamed bytes to backupDir/runs/<runID>/<stream>.log and
// returns the number of bytes written.
func (s *Store) AppendOutput(runID, stream string, data []byte) (int, error) {
	if stream != "stdout" && stream != "stderr" {
		stream = "stdout"
	}
	dir := filepath.Join(s.backupDir, "runs", runID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(filepath.Join(dir, stream+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return f.Write(data)
}

// OutputPath returns where a run's stdout is stored (for the UI/inspection).
func (s *Store) OutputPath(runID string) string {
	return filepath.Join(s.backupDir, "runs", runID, "stdout.log")
}
