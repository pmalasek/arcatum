package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Schedules are the "when" half of a backup, kept apart from the instance that says
// "what". An instance may have several — a nightly dump and a full copy once a month are
// two schedules of one task — and each can be paused on its own.

// scheduleState summarises a schedule for the list: is it running right now, did it last
// finish cleanly, is it paused. Deciding it here rather than in the browser keeps one
// rule in one place; the UI only has to colour a badge.
type scheduleState string

const (
	scheduleRunning scheduleState = "running" // a run of this task is in flight
	scheduleOK      scheduleState = "ok"      // the last run succeeded
	scheduleFailed  scheduleState = "failed"  // the last run did not
	scheduleNever   scheduleState = "never"   // it has not run yet
	schedulePaused  scheduleState = "paused"  // disabled; it is not going to run
)

// scheduleView is a schedule plus everything the Schedules tab shows next to it, so the
// tab is one request rather than one per row.
type scheduleView struct {
	*Schedule
	Script   string        `json:"script,omitempty"`
	RunnerID string        `json:"runner_id,omitempty"`
	NextRun  *time.Time    `json:"next_run,omitempty"`
	LastRun  *Run          `json:"last_run,omitempty"`
	State    scheduleState `json:"state"`
}

// schedulePayload is what the API accepts for a schedule.
type schedulePayload struct {
	InstanceID string   `json:"instance_id"`
	Name       string   `json:"name"`
	Frequency  string   `json:"frequency"`
	Time       string   `json:"time"`
	Weekdays   []string `json:"weekdays"`
	Day        int      `json:"day"`
	Timezone   string   `json:"timezone"`
	// Enabled defaults to true for a create: a schedule somebody has just filled in is
	// meant to run. Sent explicitly it is honoured, which is how the pause button works.
	Enabled *bool `json:"enabled"`
}

// handleListSchedules returns every schedule with its instance, next run, last run and
// state.
func (s *Server) handleListSchedules(w http.ResponseWriter, r *http.Request) {
	views, err := s.scheduleViews("")
	if err != nil {
		s.log.Printf("list schedules: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, views)
}

// handleInstanceSchedules returns the schedules of one instance.
func (s *Server) handleInstanceSchedules(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	views, err := s.scheduleViews(id)
	if err != nil {
		s.log.Printf("schedules for instance %q: %v", id, err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, views)
}

// handleScheduleDetail returns one schedule.
func (s *Server) handleScheduleDetail(w http.ResponseWriter, r *http.Request) {
	sc, err := s.store.ScheduleByID(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if sc == nil {
		writeError(w, http.StatusNotFound, "unknown schedule")
		return
	}
	writeJSON(w, sc)
}

// handleCreateSchedule adds a schedule to an instance (admin only).
func (s *Server) handleCreateSchedule(w http.ResponseWriter, r *http.Request) {
	var p schedulePayload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<18)).Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, "bad request: "+err.Error())
		return
	}
	sc, err := s.scheduleFromPayload(p, "")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	saved, err := s.store.CreateSchedule(sc, time.Now())
	if err != nil {
		s.log.Printf("create schedule: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := s.sched.TrackSchedule(saved, time.Now()); err != nil {
		// Validation already parsed this spec, so getting here means something changed
		// underneath us; the row is stored either way and a restart would pick it up.
		s.log.Printf("schedule %s: track: %v", saved.ID, err)
	}
	s.log.Printf("schedule %s created for instance %q (%s)", saved.ID, saved.InstanceID, describeSchedule(saved))
	writeJSONStatus(w, http.StatusCreated, saved)
}

// handleUpdateSchedule replaces a schedule's timing or pauses it (admin only).
func (s *Server) handleUpdateSchedule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, err := s.store.ScheduleByID(id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "unknown schedule")
		return
	}
	var p schedulePayload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<18)).Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, "bad request: "+err.Error())
		return
	}
	// A schedule does not move between instances: the runs already recorded against it
	// would move with it and stop making sense. The stored owner wins over the payload.
	p.InstanceID = existing.InstanceID
	// A pause is a one-field request; the rest of the schedule comes from what is stored.
	if p.Frequency == "" && p.Time == "" && p.Enabled != nil {
		p.Frequency, p.Time = existing.Frequency, existing.Time
		p.Weekdays, p.Day, p.Timezone = existing.Weekdays, existing.Day, existing.Timezone
		if p.Name == "" {
			p.Name = existing.Name
		}
	}
	sc, err := s.scheduleFromPayload(p, id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.UpdateSchedule(sc, time.Now()); err != nil {
		s.scheduleStoreError(w, err)
		return
	}
	// Re-tracking recomputes the next run, so a changed schedule takes effect at once
	// rather than at the next restart.
	if err := s.sched.TrackSchedule(sc, time.Now()); err != nil {
		s.log.Printf("schedule %s: track: %v", sc.ID, err)
	}
	s.log.Printf("schedule %s updated (%s)", sc.ID, describeSchedule(sc))
	writeJSON(w, sc)
}

// handleDeleteSchedule removes a schedule (admin only). The instance and its backups are
// untouched — it simply stops running on its own.
func (s *Server) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteSchedule(id); err != nil {
		s.scheduleStoreError(w, err)
		return
	}
	s.sched.UntrackSchedule(id)
	s.log.Printf("schedule %s deleted", id)
	writeJSON(w, map[string]string{"status": "deleted", "schedule": id})
}

// handleInstanceRuns returns one task's run history, newest first.
func (s *Server) handleInstanceRuns(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	limit := intParam(r, "limit", 50, 500)
	offset := intParam(r, "offset", 0, 1<<20)
	runs, hasMore, err := s.store.RunsForInstance(id, limit, offset)
	if err != nil {
		s.log.Printf("runs for instance %q: %v", id, err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if runs == nil {
		runs = []*Run{}
	}
	writeJSON(w, map[string]any{
		"instance_id": id,
		"runs":        runs,
		"limit":       limit,
		"offset":      offset,
		"has_more":    hasMore,
	})
}

// intParam reads a non-negative integer query parameter, falling back to def and capped
// at max. A caller asking for a million rows is a mistake or an attack; either way the
// answer is the same as asking for the cap.
func intParam(r *http.Request, name string, def, max int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// scheduleViews assembles the list the Schedules tab renders. instanceID limits it to
// one task; empty means all of them.
func (s *Server) scheduleViews(instanceID string) ([]scheduleView, error) {
	var schedules []*Schedule
	var err error
	if instanceID == "" {
		schedules, err = s.store.Schedules()
	} else {
		schedules, err = s.store.SchedulesForInstance(instanceID)
	}
	if err != nil {
		return nil, err
	}
	instances, err := s.store.Instances()
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*Instance, len(instances))
	for _, in := range instances {
		byID[in.ID] = in
	}
	lastBySchedule, err := s.store.LastRunPerSchedule()
	if err != nil {
		return nil, err
	}
	// "Running" is a property of the task, not of one of its schedules: whichever
	// arrangement started it, the repository is busy and the next one has to wait.
	unfinished, err := s.store.UnfinishedRuns()
	if err != nil {
		return nil, err
	}
	busy := map[string]bool{}
	for _, run := range unfinished {
		busy[run.InstanceID] = true
	}

	out := make([]scheduleView, 0, len(schedules))
	for _, sc := range schedules {
		v := scheduleView{Schedule: sc, LastRun: lastBySchedule[sc.ID]}
		if in := byID[sc.InstanceID]; in != nil {
			v.Script, v.RunnerID = in.Script, in.RunnerID
		}
		if next, ok := s.sched.NextRun(sc.ID); ok {
			v.NextRun = &next
		}
		v.State = deriveScheduleState(sc, v.LastRun, busy[sc.InstanceID])
		out = append(out, v)
	}
	return out, nil
}

// deriveScheduleState folds "paused", "in flight" and "how did it last go" into the one
// word an operator actually scans the column for.
func deriveScheduleState(sc *Schedule, last *Run, busy bool) scheduleState {
	switch {
	case !sc.Enabled:
		// A paused schedule reports paused even mid-run: what the column answers is "will
		// this run again", and for a paused one the answer is no whatever is happening now.
		return schedulePaused
	case busy:
		return scheduleRunning
	case last == nil:
		return scheduleNever
	case last.Status == StatusSuccess:
		return scheduleOK
	case last.Status == StatusCancelled:
		// Somebody stopped it on purpose. That is not a fault to chase.
		return scheduleNever
	default:
		return scheduleFailed
	}
}

// scheduleFromPayload validates a payload and turns it into a schedule. id is empty for
// a create and the path's id for an update.
func (s *Server) scheduleFromPayload(p schedulePayload, id string) (*Schedule, error) {
	if p.InstanceID == "" {
		return nil, fmt.Errorf("instance_id is required")
	}
	in, err := s.store.Instance(p.InstanceID)
	if err != nil {
		return nil, err
	}
	if in == nil {
		return nil, fmt.Errorf("unknown instance %q", p.InstanceID)
	}
	// Checked by name rather than left to Spec: an unknown frequency parses happily and
	// only fails later, inside Next, as "no run time found within range" — which would
	// leave a schedule that looks saved and never runs.
	switch p.Frequency {
	case "daily", "weekly", "monthly":
	default:
		return nil, fmt.Errorf("frequency must be daily, weekly or monthly, got %q", p.Frequency)
	}
	if p.Frequency == "monthly" && (p.Day < 1 || p.Day > 28) {
		// Above 28 a monthly schedule would skip February entirely.
		return nil, fmt.Errorf("a monthly schedule needs a day between 1 and 28, got %d", p.Day)
	}
	name := strings.TrimSpace(p.Name)
	if len(name) > 64 {
		return nil, fmt.Errorf("name must be at most 64 characters")
	}
	weekdays := make([]string, 0, len(p.Weekdays))
	for _, wd := range p.Weekdays {
		if wd = strings.TrimSpace(wd); wd != "" {
			weekdays = append(weekdays, wd)
		}
	}
	enabled := true
	if p.Enabled != nil {
		enabled = *p.Enabled
	}
	sc := &Schedule{
		ID:         id,
		InstanceID: p.InstanceID,
		Name:       name,
		ScheduleJSON: ScheduleJSON{
			Frequency: p.Frequency,
			Time:      strings.TrimSpace(p.Time),
			Weekdays:  weekdays,
			Day:       p.Day,
			Timezone:  strings.TrimSpace(p.Timezone),
		},
		Enabled: enabled,
	}
	// Validate exactly what tracking will do, not a subset of it: a spec that parses but
	// yields no next run is a schedule that never fires.
	spec, err := sc.Spec(s.sched.Location())
	if err != nil {
		return nil, err
	}
	if _, err := spec.Next(time.Now()); err != nil {
		return nil, fmt.Errorf("schedule never comes due: %w", err)
	}
	return sc, nil
}

// describeSchedule renders a schedule for the log in the form an operator reads it.
func describeSchedule(sc *Schedule) string {
	out := sc.Frequency + " " + sc.Time
	switch {
	case sc.Frequency == "weekly" && len(sc.Weekdays) > 0:
		out = strings.Join(sc.Weekdays, ",") + " " + sc.Time
	case sc.Frequency == "monthly":
		out = fmt.Sprintf("day %d %s", sc.Day, sc.Time)
	}
	if sc.Timezone != "" {
		out += " " + sc.Timezone
	}
	if !sc.Enabled {
		out += " (paused)"
	}
	return out
}

func (s *Server) scheduleStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrScheduleNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	default:
		s.log.Printf("schedule store: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}
