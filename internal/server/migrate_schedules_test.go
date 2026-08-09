package server

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// The migration that lifts inline schedules into their own table runs on every open, so
// what it must get right is not the first pass but the second: it has to do nothing at
// all, and above all it must not resurrect what an operator has deleted.

// openAt opens a store on a fixed path, so the same database can be closed and reopened
// the way a server restart does.
func openAt(t *testing.T, path string) *Store {
	t.Helper()
	st, err := Open(path, filepath.Join(filepath.Dir(path), "backup"), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return st
}

// writeLegacyInstance inserts a row the way a pre-split server would have: timing inline
// in the schedule column, and not yet migrated.
func writeLegacyInstance(t *testing.T, st *Store, id, schedule string) {
	t.Helper()
	_, err := st.db.Exec(`
		INSERT INTO instances (id, script, runner_id, params, secrets, capture, timeout,
		  schedule, schedule_migrated)
		VALUES (?, 'hello', 'host-1', '{}', '{}', '', '', ?, 0)`, id, schedule)
	if err != nil {
		t.Fatalf("insert legacy instance %q: %v", id, err)
	}
}

func countSchedules(t *testing.T, st *Store, instanceID string) int {
	t.Helper()
	list, err := st.SchedulesForInstance(instanceID)
	if err != nil {
		t.Fatalf("SchedulesForInstance: %v", err)
	}
	return len(list)
}

func TestMigrateInlineSchedules(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	st := openAt(t, path)
	writeLegacyInstance(t, st, "mysql-web01", `{"frequency":"weekly","time":"02:00","weekdays":["mon","thu"],"timezone":"Europe/Prague"}`)
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A restart is what actually runs the migration.
	st = openAt(t, path)
	list, err := st.SchedulesForInstance("mysql-web01")
	if err != nil || len(list) != 1 {
		t.Fatalf("SchedulesForInstance = %v, %v; want exactly one schedule", list, err)
	}
	sc := list[0]
	if sc.Frequency != "weekly" || sc.Time != "02:00" || sc.Timezone != "Europe/Prague" {
		t.Errorf("migrated schedule = %+v, want the inline one verbatim", sc)
	}
	if len(sc.Weekdays) != 2 || sc.Weekdays[0] != "mon" || sc.Weekdays[1] != "thu" {
		t.Errorf("weekdays = %v, want [mon thu]", sc.Weekdays)
	}
	if !sc.Enabled {
		t.Error("a migrated schedule must arrive enabled — it was running before the upgrade")
	}

	// Another restart must change nothing. Without the per-row marker this is where a
	// second copy of every schedule would appear.
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	st = openAt(t, path)
	if n := countSchedules(t, st, "mysql-web01"); n != 1 {
		t.Fatalf("after a second restart there are %d schedules, want 1", n)
	}

	// The case the marker exists for: an operator deletes the schedule, and a restart must
	// leave it deleted. Bringing it back would run the task at a time somebody removed.
	if err := st.DeleteSchedule(list[0].ID); err != nil {
		t.Fatalf("DeleteSchedule: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	st = openAt(t, path)
	defer st.Close()
	if n := countSchedules(t, st, "mysql-web01"); n != 0 {
		t.Errorf("a deleted schedule came back after a restart (%d rows)", n)
	}
}

// An instance whose inline schedule is empty or unreadable must not stop the server from
// starting: every other backup on the machine would stop with it. The instance keeps
// working on demand and its schedule is retyped by hand.
func TestMigrateInlineSchedulesToleratesBadInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	st := openAt(t, path)
	writeLegacyInstance(t, st, "empty", `{}`)
	writeLegacyInstance(t, st, "broken", `not json at all`)
	writeLegacyInstance(t, st, "no-time", `{"frequency":"daily"}`)
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	st = openAt(t, path)
	defer st.Close()
	for _, id := range []string{"empty", "broken", "no-time"} {
		if n := countSchedules(t, st, id); n != 0 {
			t.Errorf("instance %q got %d schedules from an unusable inline one", id, n)
		}
		// Marked all the same, so the row is not re-examined on every start for ever.
		var migrated int
		err := st.db.QueryRow(`SELECT schedule_migrated FROM instances WHERE id = ?`, id).Scan(&migrated)
		if err != nil || migrated != 1 {
			t.Errorf("instance %q: schedule_migrated = %d, %v; want 1", id, migrated, err)
		}
	}
	// The instances themselves survived, which is the point of not failing the open.
	all, err := st.Instances()
	if err != nil || len(all) != 3 {
		t.Fatalf("Instances = %d, %v; want the three to be intact", len(all), err)
	}
}

// Rows written by this version are marked migrated as they are inserted, so the scan
// never revisits them.
func TestNewInstancesAreNotRescanned(t *testing.T) {
	st, _ := openTestStore(t)
	if err := st.SaveInstance(&Instance{ID: "fresh", Script: "hello", RunnerID: "host-1"}, true); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}
	var pending int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM instances WHERE schedule_migrated = 0`).Scan(&pending); err != nil && err != sql.ErrNoRows {
		t.Fatalf("count: %v", err)
	}
	if pending != 0 {
		t.Errorf("%d instance(s) still queued for migration, want 0", pending)
	}
}
