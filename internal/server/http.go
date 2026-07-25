package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"arcatum/pkg/proto"
)

// Server wires the HTTP API to the store, scheduler and script catalog.
type Server struct {
	store   *Store
	sched   *Scheduler
	catalog *Catalog
	log     *log.Logger
}

// New builds a Server: loads the catalog, instances, and starts tracking schedules.
func New(scriptsDir, instancesPath, backupDir string, loc *time.Location, logger *log.Logger) (*Server, error) {
	cat, err := LoadCatalog(scriptsDir)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	store := NewStore(backupDir)
	if err := store.LoadInstances(instancesPath); err != nil {
		return nil, fmt.Errorf("instances: %w", err)
	}
	sched := NewScheduler(loc)
	now := time.Now()
	for _, in := range store.Instances() {
		if _, ok := cat.Get(in.Script); !ok {
			return nil, fmt.Errorf("instance %q references unknown script %q", in.ID, in.Script)
		}
		if err := sched.Track(in, now); err != nil {
			return nil, fmt.Errorf("instance %q schedule: %w", in.ID, err)
		}
	}
	return &Server{store: store, sched: sched, catalog: cat, log: logger}, nil
}

// Handler returns the HTTP router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/checkin", s.handleCheckin)
	mux.HandleFunc("POST /api/v1/runs/updates", s.handleUpdates)
	mux.HandleFunc("POST /api/v1/instances/{id}/run", s.handleTrigger)
	mux.HandleFunc("GET /api/v1/runs", s.handleListRuns)
	mux.HandleFunc("GET /", s.handleIndex)
	return mux
}

// handleCheckin returns the jobs due for the calling runner.
func (s *Server) handleCheckin(w http.ResponseWriter, r *http.Request) {
	var req proto.CheckinRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	now := time.Now()
	var due []proto.JobDispatch
	for _, in := range s.store.InstancesForRunner(req.RunnerID) {
		if !s.sched.Due(in.ID, now) {
			continue
		}
		d, err := s.buildDispatch(in)
		if err != nil {
			s.log.Printf("checkin: instance %q: %v", in.ID, err)
			continue
		}
		s.sched.MarkDispatched(in.ID, now)
		due = append(due, d)
		s.log.Printf("dispatch: instance=%s run=%s -> runner=%s", in.ID, d.RunID, req.RunnerID)
	}
	writeJSON(w, proto.CheckinResponse{Due: due})
}

// buildDispatch turns an instance into a signed-later JobDispatch and records a Run.
func (s *Server) buildDispatch(in *Instance) (proto.JobDispatch, error) {
	entry, ok := s.catalog.Get(in.Script)
	if !ok {
		return proto.JobDispatch{}, fmt.Errorf("unknown script %q", in.Script)
	}
	content, sha, err := entry.readArtifact()
	if err != nil {
		return proto.JobDispatch{}, err
	}
	run := s.store.CreateRun(in)
	capture := in.Capture
	if capture == "" {
		capture = "stream"
	}
	return proto.JobDispatch{
		RunID:      run.ID,
		InstanceID: in.ID,
		Script:     in.Script,
		Type:       entry.Manifest.Type,
		Artifact: proto.Artifact{
			Filename: entry.Manifest.Entrypoint,
			SHA256:   sha,
			Content:  content,
		},
		Params:     in.Params,
		Secrets:    in.Secrets,
		TimeoutSec: timeoutSeconds(in.Timeout, entry.Manifest.Timeout),
		Capture:    capture,
	}, nil
}

// handleUpdates consumes a runner's ndjson stream of RunUpdate for one run.
func (s *Server) handleUpdates(w http.ResponseWriter, r *http.Request) {
	dec := json.NewDecoder(r.Body)
	for {
		var u proto.RunUpdate
		if err := dec.Decode(&u); err != nil {
			break // EOF or malformed tail — stop consuming
		}
		s.applyUpdate(u)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) applyUpdate(u proto.RunUpdate) {
	switch u.Kind {
	case proto.KindStarted:
		s.store.UpdateRun(u.RunID, func(r *Run) {
			r.Status = StatusRunning
			r.StartedAt = time.Now()
		})
		s.log.Printf("run=%s started", u.RunID)
	case proto.KindOutput:
		n, err := s.store.AppendOutput(u.RunID, u.Stream, u.Data)
		if err != nil {
			s.log.Printf("run=%s output write: %v", u.RunID, err)
			return
		}
		s.store.UpdateRun(u.RunID, func(r *Run) { r.Bytes += int64(n) })
	case proto.KindFinished:
		s.store.UpdateRun(u.RunID, func(r *Run) {
			r.EndedAt = time.Now()
			r.ExitCode = u.ExitCode
			if u.Error != "" {
				r.Status = StatusError
				r.Err = u.Error
			} else if u.ExitCode == 0 {
				r.Status = StatusSuccess
			} else {
				r.Status = StatusFailed
			}
		})
		s.log.Printf("run=%s finished exit=%d err=%q", u.RunID, u.ExitCode, u.Error)
	}
}

// handleTrigger marks an instance for an immediate run (web "run now").
func (s *Server) handleTrigger(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.sched.Trigger(id) {
		http.Error(w, "unknown instance", http.StatusNotFound)
		return
	}
	s.log.Printf("trigger: instance=%s queued for next check-in", id)
	writeJSON(w, map[string]string{"status": "queued", "instance": id})
}

// handleListRuns returns runs newest-first.
func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.store.ListRuns())
}

// handleIndex is a minimal text status page (real web UI later).
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "arcatum-server\n\nscripts: %v\ninstances: %d\n\nruns (newest first):\n",
		s.catalog.Names(), len(s.store.Instances()))
	for _, run := range s.store.ListRuns() {
		fmt.Fprintf(w, "  %s  %-8s  instance=%s  exit=%d  bytes=%d\n",
			run.ID, run.Status, run.InstanceID, run.ExitCode, run.Bytes)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// timeoutSeconds resolves an instance/manifest timeout string to seconds, default 1h.
func timeoutSeconds(instTimeout, manifestTimeout string) int {
	for _, s := range []string{instTimeout, manifestTimeout} {
		if s == "" {
			continue
		}
		if d, err := time.ParseDuration(s); err == nil {
			return int(d.Seconds())
		}
	}
	return int((time.Hour).Seconds())
}
