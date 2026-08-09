package server

import (
	"testing"
	"time"
)

// The scheduler is keyed by schedule, not by instance, which is what makes several
// timings per task possible. These cover the consequences of that: what is due, what a
// paused schedule does, and how a manual run stays independent of both.

func testSchedule(id, instanceID, at string, enabled bool) *Schedule {
	return &Schedule{
		ID:           id,
		InstanceID:   instanceID,
		ScheduleJSON: ScheduleJSON{Frequency: "daily", Time: at},
		Enabled:      enabled,
	}
}

// trackedAt registers a schedule and forces its next-run to `next`, so a test does not
// have to wait for the clock.
func trackedAt(t *testing.T, s *Scheduler, sc *Schedule, next time.Time) {
	t.Helper()
	if err := s.TrackSchedule(sc, time.Now()); err != nil {
		t.Fatalf("TrackSchedule(%s): %v", sc.ID, err)
	}
	s.mu.Lock()
	s.st[sc.ID].next = next
	s.mu.Unlock()
}

// Two schedules of one task coming due together is the case the split introduces, and the
// one that must not produce two concurrent runs. The scheduler reports both as due —
// coalescing them into a single dispatch is the check-in's job — and both are advanced.
func TestDueForReportsEverySchedule(t *testing.T) {
	s := NewScheduler(time.UTC)
	now := time.Now()
	trackedAt(t, s, testSchedule("sch-1", "mysql-web01", "02:00", true), now.Add(-2*time.Minute))
	trackedAt(t, s, testSchedule("sch-2", "mysql-web01", "23:00", true), now.Add(-time.Minute))

	due, manual := s.DueFor("mysql-web01", now)
	if len(due) != 2 {
		t.Fatalf("DueFor = %v, want both schedules", due)
	}
	// Earliest first, so the run is attributed to the one that actually came due first.
	if due[0] != "sch-1" || due[1] != "sch-2" {
		t.Errorf("DueFor = %v, want [sch-1 sch-2] in next-run order", due)
	}
	if manual {
		t.Error("no manual run was requested")
	}

	for _, id := range due {
		s.MarkDispatched(id, now)
	}
	if again, _ := s.DueFor("mysql-web01", now); len(again) != 0 {
		t.Errorf("DueFor after dispatch = %v, want none — both must be advanced", again)
	}
}

// A paused schedule keeps its definition and stops coming due. It also reports no next
// run: the column answers "when does this run next", and for a paused one there is no
// answer.
func TestDisabledScheduleNeverComesDue(t *testing.T) {
	s := NewScheduler(time.UTC)
	now := time.Now()
	trackedAt(t, s, testSchedule("sch-1", "mysql-web01", "02:00", false), now.Add(-time.Hour))

	if due, _ := s.DueFor("mysql-web01", now); len(due) != 0 {
		t.Errorf("DueFor = %v, want none for a paused schedule", due)
	}
	if _, ok := s.NextRun("sch-1"); ok {
		t.Error("a paused schedule must not report a next run")
	}
	if _, ok := s.NextRunForInstance("mysql-web01"); ok {
		t.Error("an instance whose only schedule is paused has no next run")
	}
	if u := s.Upcoming(10); len(u) != 0 {
		t.Errorf("Upcoming = %v, want a paused schedule left out", u)
	}
}

// A manual run belongs to the task, not to any schedule, so it has to work for an
// instance that has none. Before the split this was impossible: an instance the scheduler
// did not know could not be triggered at all.
func TestTriggerWorksWithoutSchedules(t *testing.T) {
	s := NewScheduler(time.UTC)
	s.Trigger("no-schedules-at-all")

	due, manual := s.DueFor("no-schedules-at-all", time.Now())
	if len(due) != 0 || !manual {
		t.Fatalf("DueFor = %v, manual=%v; want no schedules but a pending manual run", due, manual)
	}
	s.ClearManual("no-schedules-at-all")
	if _, manual := s.DueFor("no-schedules-at-all", time.Now()); manual {
		t.Error("the manual run was not cleared after dispatch")
	}
}

// Deleting a task must take its schedules and its pending manual run with it, or the
// scheduler keeps dispatching for something that no longer exists.
func TestUntrackInstanceForgetsEverything(t *testing.T) {
	s := NewScheduler(time.UTC)
	now := time.Now()
	trackedAt(t, s, testSchedule("sch-1", "gone", "02:00", true), now.Add(-time.Hour))
	trackedAt(t, s, testSchedule("sch-2", "gone", "03:00", true), now.Add(-time.Hour))
	trackedAt(t, s, testSchedule("sch-3", "stays", "04:00", true), now.Add(-time.Hour))
	s.Trigger("gone")

	s.UntrackInstance("gone")
	due, manual := s.DueFor("gone", now)
	if len(due) != 0 || manual {
		t.Errorf("DueFor(gone) = %v, manual=%v; want nothing left", due, manual)
	}
	if due, _ := s.DueFor("stays", now); len(due) != 1 {
		t.Errorf("DueFor(stays) = %v, want the other instance untouched", due)
	}
}

// Untracking one schedule leaves its siblings alone — pausing or deleting the nightly run
// must not stop the monthly one.
func TestUntrackScheduleLeavesSiblings(t *testing.T) {
	s := NewScheduler(time.UTC)
	now := time.Now()
	trackedAt(t, s, testSchedule("sch-1", "mysql-web01", "02:00", true), now.Add(-time.Hour))
	trackedAt(t, s, testSchedule("sch-2", "mysql-web01", "03:00", true), now.Add(-time.Hour))

	s.UntrackSchedule("sch-1")
	due, _ := s.DueFor("mysql-web01", now)
	if len(due) != 1 || due[0] != "sch-2" {
		t.Errorf("DueFor = %v, want only sch-2 left", due)
	}
}

// Reset computes everything before it drops anything, so an import carrying one unusable
// schedule leaves the running configuration exactly as it was rather than half-replaced.
func TestResetKeepsStateWhenOneScheduleIsBad(t *testing.T) {
	s := NewScheduler(time.UTC)
	now := time.Now()
	trackedAt(t, s, testSchedule("sch-1", "mysql-web01", "02:00", true), now.Add(-time.Hour))

	bad := testSchedule("sch-9", "other", "25:00", true)
	err := s.Reset([]*Schedule{testSchedule("sch-8", "other", "01:00", true), bad}, now)
	if err == nil {
		t.Fatal("Reset accepted an unparseable schedule")
	}
	if due, _ := s.DueFor("mysql-web01", now); len(due) != 1 {
		t.Errorf("DueFor = %v, want the previous state left alone after a failed Reset", due)
	}
}

// Upcoming is what the dashboard's "next runs" card reads: soonest first, capped.
func TestUpcomingIsOrderedAndCapped(t *testing.T) {
	s := NewScheduler(time.UTC)
	now := time.Now()
	trackedAt(t, s, testSchedule("sch-late", "a", "02:00", true), now.Add(3*time.Hour))
	trackedAt(t, s, testSchedule("sch-soon", "b", "03:00", true), now.Add(time.Hour))
	trackedAt(t, s, testSchedule("sch-mid", "c", "04:00", true), now.Add(2*time.Hour))

	up := s.Upcoming(2)
	if len(up) != 2 {
		t.Fatalf("Upcoming(2) = %d entries, want 2", len(up))
	}
	if up[0].ScheduleID != "sch-soon" || up[1].ScheduleID != "sch-mid" {
		t.Errorf("Upcoming = %v, want the two soonest in order", up)
	}
	if up[0].InstanceID != "b" {
		t.Errorf("InstanceID = %q, want the schedule's own instance", up[0].InstanceID)
	}
}
