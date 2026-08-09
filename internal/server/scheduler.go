package server

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"arcatum/pkg/schedule"
)

// Scheduler tracks, per schedule, when it is next due, and per instance whether a manual
// run has been asked for. It is intentionally simple: no in-flight tracking, no
// persistence — next-run times are recomputed from the specs at startup.
//
// The two are keyed differently on purpose. A schedule is a standing arrangement and an
// instance may have several; a manual "run now" belongs to the task, not to any of its
// schedules, and has to work for an instance that has none at all.
type Scheduler struct {
	mu         sync.Mutex
	st         map[string]*schedState     // schedule id -> state
	byInstance map[string]map[string]bool // instance id -> its schedule ids
	manual     map[string]bool            // instance id -> a run was requested by hand
	loc        *time.Location
}

type schedState struct {
	instanceID string
	spec       schedule.Spec
	next       time.Time
	enabled    bool
}

// Upcoming is one scheduled run in the near future, for the dashboard.
type Upcoming struct {
	ScheduleID string    `json:"schedule_id"`
	InstanceID string    `json:"instance_id"`
	Name       string    `json:"name,omitempty"`
	NextRun    time.Time `json:"next_run"`
}

// NewScheduler creates a scheduler with a default timezone for schedules that omit one.
func NewScheduler(loc *time.Location) *Scheduler {
	return &Scheduler{
		st:         map[string]*schedState{},
		byInstance: map[string]map[string]bool{},
		manual:     map[string]bool{},
		loc:        loc,
	}
}

// Location is the default timezone schedules fall back to. Validating a schedule without
// tracking it needs the same location the scheduler would use, so a schedule that parses
// here is one TrackSchedule will accept.
func (s *Scheduler) Location() *time.Location { return s.loc }

// TrackSchedule registers a schedule and computes its first next-run after `now`.
//
// A disabled schedule is tracked too, and its next-run computed: enabling it is then a
// flag, not a re-parse that could fail at the worst moment. Due and NextRun ignore it
// while it is off.
func (s *Scheduler) TrackSchedule(sc *Schedule, now time.Time) error {
	spec, err := sc.Spec(s.loc)
	if err != nil {
		return err
	}
	next, err := spec.Next(now)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trackLocked(sc, spec, next)
	return nil
}

func (s *Scheduler) trackLocked(sc *Schedule, spec schedule.Spec, next time.Time) {
	// A schedule that was already tracked may have belonged to another instance in a
	// previous life only through a bug; still, drop the old index entry rather than leave
	// a dangling one.
	if old, ok := s.st[sc.ID]; ok && old.instanceID != sc.InstanceID {
		delete(s.byInstance[old.instanceID], sc.ID)
	}
	s.st[sc.ID] = &schedState{
		instanceID: sc.InstanceID,
		spec:       spec,
		next:       next,
		enabled:    sc.Enabled,
	}
	if s.byInstance[sc.InstanceID] == nil {
		s.byInstance[sc.InstanceID] = map[string]bool{}
	}
	s.byInstance[sc.InstanceID][sc.ID] = true
}

// UntrackSchedule forgets one schedule, so a deleted one stops firing without a restart.
func (s *Scheduler) UntrackSchedule(scheduleID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.st[scheduleID]
	if !ok {
		return
	}
	delete(s.byInstance[st.instanceID], scheduleID)
	if len(s.byInstance[st.instanceID]) == 0 {
		delete(s.byInstance, st.instanceID)
	}
	delete(s.st, scheduleID)
}

// UntrackInstance forgets every schedule of an instance along with any pending manual
// run. A deleted task must not leave behind a schedule that keeps coming due.
func (s *Scheduler) UntrackInstance(instanceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.byInstance[instanceID] {
		delete(s.st, id)
	}
	delete(s.byInstance, instanceID)
	delete(s.manual, instanceID)
}

// DueFor reports which of an instance's schedules have come due at `now`, earliest
// first, and whether a manual run is pending. Disabled schedules are never due.
func (s *Scheduler) DueFor(instanceID string, now time.Time) (due []string, manual bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.byInstance[instanceID] {
		st := s.st[id]
		if st == nil || !st.enabled {
			continue
		}
		if !now.Before(st.next) {
			due = append(due, id)
		}
	}
	sort.Slice(due, func(i, j int) bool { return s.st[due[i]].next.Before(s.st[due[j]].next) })
	return due, s.manual[instanceID]
}

// MarkDispatched advances one schedule's next-run past `now`.
func (s *Scheduler) MarkDispatched(scheduleID string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.st[scheduleID]
	if !ok {
		return
	}
	if next, err := st.spec.Next(now); err == nil {
		st.next = next
	}
}

// Trigger requests an immediate run of an instance on its next check-in. This backs the
// web UI's "run now" button.
//
// It takes no view on whether the instance exists — that is the store's business, and
// the handler has already asked. Deciding it here used to mean an instance whose
// schedule failed to parse could not be run by hand either, which is exactly the
// instance somebody most wants to test.
func (s *Scheduler) Trigger(instanceID string) {
	s.mu.Lock()
	s.manual[instanceID] = true
	s.mu.Unlock()
}

// ClearManual drops a pending manual run once it has been dispatched.
func (s *Scheduler) ClearManual(instanceID string) {
	s.mu.Lock()
	delete(s.manual, instanceID)
	s.mu.Unlock()
}

// NextRun returns the tracked next-run time for one schedule. A disabled schedule has
// none to report: it is not going to run.
func (s *Scheduler) NextRun(scheduleID string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.st[scheduleID]
	if !ok || !st.enabled {
		return time.Time{}, false
	}
	return st.next, true
}

// NextRunForInstance returns the earliest next-run across an instance's enabled
// schedules — when this task next runs, whichever arrangement brings it about.
func (s *Scheduler) NextRunForInstance(instanceID string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best time.Time
	for id := range s.byInstance[instanceID] {
		st := s.st[id]
		if st == nil || !st.enabled {
			continue
		}
		if best.IsZero() || st.next.Before(best) {
			best = st.next
		}
	}
	return best, !best.IsZero()
}

// Upcoming returns the next n scheduled runs across everything, soonest first.
func (s *Scheduler) Upcoming(n int) []Upcoming {
	s.mu.Lock()
	out := make([]Upcoming, 0, len(s.st))
	for id, st := range s.st {
		if !st.enabled {
			continue
		}
		out = append(out, Upcoming{ScheduleID: id, InstanceID: st.instanceID, NextRun: st.next})
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].NextRun.Before(out[j].NextRun) })
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

// Reset replaces everything tracked with exactly this set of schedules. It exists for
// the configuration import, which does not edit schedules one by one but swaps the whole
// set: untracking the old ones individually would leave whatever the import removed
// still on the schedule.
//
// The specs are computed before anything is dropped, so a schedule that will not parse
// fails the call and leaves the previous state alone.
func (s *Scheduler) Reset(schedules []*Schedule, now time.Time) error {
	type tracked struct {
		sc   *Schedule
		spec schedule.Spec
		next time.Time
	}
	prepared := make([]tracked, 0, len(schedules))
	for _, sc := range schedules {
		spec, err := sc.Spec(s.loc)
		if err != nil {
			return fmt.Errorf("schedule %s (instance %q): %w", sc.ID, sc.InstanceID, err)
		}
		at, err := spec.Next(now)
		if err != nil {
			return fmt.Errorf("schedule %s (instance %q): %w", sc.ID, sc.InstanceID, err)
		}
		prepared = append(prepared, tracked{sc: sc, spec: spec, next: at})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st = make(map[string]*schedState, len(prepared))
	s.byInstance = make(map[string]map[string]bool, len(prepared))
	s.manual = map[string]bool{}
	for _, t := range prepared {
		s.trackLocked(t.sc, t.spec, t.next)
	}
	return nil
}
