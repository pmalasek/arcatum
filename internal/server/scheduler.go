package server

import (
	"fmt"
	"sync"
	"time"

	"arcatum/pkg/schedule"
)

// Scheduler tracks, per instance, when it is next due and whether a manual run was
// requested. It is intentionally simple: no in-flight tracking, no persistence.
type Scheduler struct {
	mu  sync.Mutex
	st  map[string]*schedState
	loc *time.Location
}

type schedState struct {
	spec   schedule.Spec
	next   time.Time
	manual bool
}

// NewScheduler creates a scheduler with a default timezone for instances that omit one.
func NewScheduler(loc *time.Location) *Scheduler {
	return &Scheduler{st: map[string]*schedState{}, loc: loc}
}

// Location is the default timezone instances fall back to. Validating a schedule
// without tracking it needs the same location the scheduler would use, so a schedule
// that parses here is one Track will accept.
func (s *Scheduler) Location() *time.Location { return s.loc }

// Track registers an instance and computes its first next-run after `now`.
func (s *Scheduler) Track(inst *Instance, now time.Time) error {
	spec, err := inst.Schedule.Spec(s.loc)
	if err != nil {
		return err
	}
	next, err := spec.Next(now)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.st[inst.ID] = &schedState{spec: spec, next: next}
	s.mu.Unlock()
	return nil
}

// Due reports whether an instance should run at `now` (schedule elapsed or a manual
// trigger is pending).
func (s *Scheduler) Due(instanceID string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.st[instanceID]
	if !ok {
		return false
	}
	return st.manual || !now.Before(st.next)
}

// MarkDispatched clears any manual flag and advances next-run past `now`.
func (s *Scheduler) MarkDispatched(instanceID string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.st[instanceID]
	if !ok {
		return
	}
	st.manual = false
	if next, err := st.spec.Next(now); err == nil {
		st.next = next
	}
}

// Untrack forgets an instance, so a deleted one stops being scheduled without needing a
// restart.
func (s *Scheduler) Untrack(instanceID string) {
	s.mu.Lock()
	delete(s.st, instanceID)
	s.mu.Unlock()
}

// Reset replaces everything tracked with exactly this set of instances. It exists for
// the configuration import, which does not edit instances one by one but swaps the
// whole set: untracking the old ones individually would leave whatever the import
// removed still on the schedule.
//
// The specs are computed before anything is dropped, so an instance with an unparseable
// schedule fails the call and leaves the previous state alone.
func (s *Scheduler) Reset(instances []*Instance, now time.Time) error {
	next := make(map[string]*schedState, len(instances))
	for _, in := range instances {
		spec, err := in.Schedule.Spec(s.loc)
		if err != nil {
			return fmt.Errorf("instance %q schedule: %w", in.ID, err)
		}
		at, err := spec.Next(now)
		if err != nil {
			return fmt.Errorf("instance %q schedule: %w", in.ID, err)
		}
		next[in.ID] = &schedState{spec: spec, next: at}
	}
	s.mu.Lock()
	s.st = next
	s.mu.Unlock()
	return nil
}

// Trigger requests an immediate run of an instance on its next check-in. This backs
// the web UI's "run now" button (script debugging).
func (s *Scheduler) Trigger(instanceID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.st[instanceID]
	if !ok {
		return false
	}
	st.manual = true
	return true
}

// NextRun returns the tracked next-run time for an instance.
func (s *Scheduler) NextRun(instanceID string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.st[instanceID]
	if !ok {
		return time.Time{}, false
	}
	return st.next, true
}
