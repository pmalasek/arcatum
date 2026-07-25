package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
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

// New builds a Server over an open Store: loads the script catalog and starts
// tracking the schedule of every instance currently in the database.
func New(store *Store, scriptsDir string, loc *time.Location, logger *log.Logger) (*Server, error) {
	cat, err := LoadCatalog(scriptsDir)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	instances, err := store.Instances()
	if err != nil {
		return nil, fmt.Errorf("load instances: %w", err)
	}
	sched := NewScheduler(loc)
	now := time.Now()
	for _, in := range instances {
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
	mux.HandleFunc("GET /api/v1/instances", s.handleListInstances)
	mux.HandleFunc("GET /api/v1/runs", s.handleListRuns)
	mux.HandleFunc("GET /api/v1/runs/{id}/output", s.handleRunOutput)
	mux.HandleFunc("GET /api/v1/runners", s.handleListRunners)
	mux.HandleFunc("GET /", s.handleIndex)
	return mux
}

// handleCheckin registers the runner and returns the jobs due for it.
func (s *Server) handleCheckin(w http.ResponseWriter, r *http.Request) {
	var req proto.CheckinRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.RunnerID == "" {
		http.Error(w, "runner_id is required", http.StatusBadRequest)
		return
	}
	now := time.Now()
	if err := s.store.RecordCheckin(req, now); err != nil {
		s.log.Printf("checkin: record runner %q: %v", req.RunnerID, err)
	}

	instances, err := s.store.InstancesForRunner(req.RunnerID)
	if err != nil {
		s.log.Printf("checkin: instances for %q: %v", req.RunnerID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var due []proto.JobDispatch
	for _, in := range instances {
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

// buildDispatch turns an instance into a JobDispatch and records a pending Run.
// Signing is added with pkg/crypto.
func (s *Server) buildDispatch(in *Instance) (proto.JobDispatch, error) {
	entry, ok := s.catalog.Get(in.Script)
	if !ok {
		return proto.JobDispatch{}, fmt.Errorf("unknown script %q", in.Script)
	}
	content, sha, err := entry.readArtifact()
	if err != nil {
		return proto.JobDispatch{}, err
	}
	run, err := s.store.CreateRun(in)
	if err != nil {
		return proto.JobDispatch{}, fmt.Errorf("create run: %w", err)
	}
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
	var err error
	switch u.Kind {
	case proto.KindStarted:
		err = s.store.MarkRunStarted(u.RunID, time.Now())
		if err == nil {
			s.log.Printf("run=%s started", u.RunID)
		}
	case proto.KindOutput:
		var n int
		n, err = s.store.AppendOutput(u.RunID, u.Stream, u.Data)
		if err == nil {
			err = s.store.AddRunBytes(u.RunID, int64(n))
		}
	case proto.KindFinished:
		err = s.store.FinishRun(u.RunID, time.Now(), u.ExitCode, u.Error)
		if err == nil {
			s.log.Printf("run=%s finished exit=%d err=%q", u.RunID, u.ExitCode, u.Error)
		}
	}
	if err != nil {
		s.log.Printf("run=%s update %s: %v", u.RunID, u.Kind, err)
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

// handleListInstances returns instances with their next scheduled run.
func (s *Server) handleListInstances(w http.ResponseWriter, r *http.Request) {
	instances, err := s.store.Instances()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	type item struct {
		*Instance
		NextRun *time.Time `json:"next_run,omitempty"`
	}
	out := make([]item, 0, len(instances))
	for _, in := range instances {
		it := item{Instance: in.Redacted()} // never expose secret values over the API
		if next, ok := s.sched.NextRun(in.ID); ok {
			it.NextRun = &next
		}
		out = append(out, it)
	}
	writeJSON(w, out)
}

// handleListRuns returns runs newest-first; ?limit=N caps the result.
func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	runs, err := s.store.ListRuns(limit)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, runs)
}

// handleRunOutput serves a run's captured output (?stream=stdout|stderr). This backs
// inspecting a run while debugging a script.
func (s *Server) handleRunOutput(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	run, err := s.store.Run(runID)
	if err != nil || run == nil {
		http.Error(w, "unknown run", http.StatusNotFound)
		return
	}
	data, err := os.ReadFile(s.store.StreamPath(runID, r.URL.Query().Get("stream")))
	if os.IsNotExist(err) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		return // no output captured (yet)
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(data)
}

// handleListRunners returns known runners, most recently seen first.
func (s *Server) handleListRunners(w http.ResponseWriter, r *http.Request) {
	runners, err := s.store.Runners()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, runners)
}

// handleIndex is a minimal text status page (real web UI later).
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "arcatum-server\n\nscripts: %v\n", s.catalog.Names())

	if runners, err := s.store.Runners(); err == nil {
		fmt.Fprintf(w, "\nrunners:\n")
		for _, rn := range runners {
			fmt.Fprintf(w, "  %-20s %s/%s  last_seen=%s\n", rn.ID, rn.OS, rn.Arch,
				rn.LastSeen.Format(time.RFC3339))
		}
	}
	if instances, err := s.store.Instances(); err == nil {
		fmt.Fprintf(w, "\ninstances:\n")
		for _, in := range instances {
			next := "-"
			if t, ok := s.sched.NextRun(in.ID); ok {
				next = t.Format(time.RFC3339)
			}
			fmt.Fprintf(w, "  %-20s script=%-14s runner=%-16s next_run=%s\n",
				in.ID, in.Script, in.RunnerID, next)
		}
	}
	if runs, err := s.store.ListRuns(20); err == nil {
		fmt.Fprintf(w, "\nruns (newest first):\n")
		for _, run := range runs {
			fmt.Fprintf(w, "  %-8s %-8s instance=%-16s exit=%-3d bytes=%d\n",
				run.ID, run.Status, run.InstanceID, run.ExitCode, run.Bytes)
		}
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
