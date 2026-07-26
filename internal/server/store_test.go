package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"arcatum/pkg/proto"
)

// openTestStore returns a Store backed by a temp DB and backup dir.
func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "test.db"), filepath.Join(dir, "backup"), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st, dir
}

func writeInstances(t *testing.T, dir string, list []*Instance) string {
	t.Helper()
	path := filepath.Join(dir, "instances.json")
	data, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestImportInstancesUpserts(t *testing.T) {
	st, dir := openTestStore(t)

	path := writeInstances(t, dir, []*Instance{{
		ID:       "mysql-web01",
		Script:   "mysql-backup",
		RunnerID: "web-01",
		Params:   map[string]string{"host": "127.0.0.1", "database": "shop"},
		Secrets:  map[string]string{"password": "p1"},
		Schedule: ScheduleJSON{Frequency: "weekly", Time: "02:30", Weekdays: []string{"mon", "thu"}},
	}})
	if n, err := st.ImportInstances(path); err != nil || n != 1 {
		t.Fatalf("ImportInstances = %d, %v; want 1, nil", n, err)
	}

	got, err := st.Instance("mysql-web01")
	if err != nil || got == nil {
		t.Fatalf("Instance: %v (nil=%v)", err, got == nil)
	}
	if got.Params["database"] != "shop" || got.Secrets["password"] != "p1" {
		t.Errorf("params/secrets round-trip failed: %+v / %+v", got.Params, got.Secrets)
	}
	if got.Schedule.Frequency != "weekly" || len(got.Schedule.Weekdays) != 2 {
		t.Errorf("schedule round-trip failed: %+v", got.Schedule)
	}

	// Re-importing the same id must update, not duplicate.
	path = writeInstances(t, dir, []*Instance{{
		ID: "mysql-web01", Script: "mysql-backup", RunnerID: "web-02",
		Params:   map[string]string{"database": "orders"},
		Schedule: ScheduleJSON{Frequency: "daily", Time: "03:00"},
	}})
	if _, err := st.ImportInstances(path); err != nil {
		t.Fatalf("re-import: %v", err)
	}
	all, err := st.Instances()
	if err != nil {
		t.Fatalf("Instances: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("got %d instances, want 1 (upsert, not insert)", len(all))
	}
	if all[0].RunnerID != "web-02" || all[0].Params["database"] != "orders" {
		t.Errorf("upsert did not apply: %+v", all[0])
	}
}

func TestImportInstancesMissingFileIsOK(t *testing.T) {
	st, dir := openTestStore(t)
	n, err := st.ImportInstances(filepath.Join(dir, "nope.json"))
	if err != nil || n != 0 {
		t.Fatalf("ImportInstances(missing) = %d, %v; want 0, nil", n, err)
	}
}

func TestInstancesForRunner(t *testing.T) {
	st, dir := openTestStore(t)
	path := writeInstances(t, dir, []*Instance{
		{ID: "a", Script: "hello", RunnerID: "host-1", Schedule: ScheduleJSON{Frequency: "daily", Time: "01:00"}},
		{ID: "b", Script: "hello", RunnerID: "host-2", Schedule: ScheduleJSON{Frequency: "daily", Time: "01:00"}},
		{ID: "c", Script: "hello", RunnerID: "host-1", Schedule: ScheduleJSON{Frequency: "daily", Time: "01:00"}},
	})
	if _, err := st.ImportInstances(path); err != nil {
		t.Fatalf("import: %v", err)
	}
	got, err := st.InstancesForRunner("host-1")
	if err != nil {
		t.Fatalf("InstancesForRunner: %v", err)
	}
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "c" {
		t.Errorf("got %d instances %v, want a and c", len(got), ids(got))
	}
}

func ids(list []*Instance) []string {
	out := make([]string, len(list))
	for i, in := range list {
		out[i] = in.ID
	}
	return out
}

func TestRunLifecycle(t *testing.T) {
	st, _ := openTestStore(t)
	inst := &Instance{ID: "hello-demo", Script: "hello", RunnerID: "host-1"}

	run, err := st.CreateRun(inst)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if run.ID != "run-1" || run.Status != StatusPending {
		t.Fatalf("CreateRun = %s/%s, want run-1/pending", run.ID, run.Status)
	}

	now := time.Now()
	if err := st.MarkRunStarted(run.ID, now); err != nil {
		t.Fatalf("MarkRunStarted: %v", err)
	}
	if err := st.AddRunBytes(run.ID, 100); err != nil {
		t.Fatalf("AddRunBytes: %v", err)
	}
	if err := st.AddRunBytes(run.ID, 23); err != nil {
		t.Fatalf("AddRunBytes: %v", err)
	}
	got, err := st.Run(run.ID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Status != StatusRunning {
		t.Errorf("status = %s, want running", got.Status)
	}
	if got.Bytes != 123 {
		t.Errorf("bytes = %d, want 123 (accumulated)", got.Bytes)
	}
	if got.StartedAt.IsZero() {
		t.Error("started_at not recorded")
	}

	if err := st.FinishRun(run.ID, now.Add(time.Second), 0, ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	got, _ = st.Run(run.ID)
	if got.Status != StatusSuccess {
		t.Errorf("status = %s, want success", got.Status)
	}
}

func TestFinishRunStatusMapping(t *testing.T) {
	st, _ := openTestStore(t)
	inst := &Instance{ID: "i", Script: "hello", RunnerID: "h"}
	tests := []struct {
		name     string
		exitCode int
		execErr  string
		want     RunStatus
	}{
		{"clean exit", 0, "", StatusSuccess},
		{"non-zero exit", 2, "", StatusFailed},
		{"runner error wins", 0, "artifact hash mismatch", StatusError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			run, err := st.CreateRun(inst)
			if err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
			if err := st.FinishRun(run.ID, time.Now(), tc.exitCode, tc.execErr); err != nil {
				t.Fatalf("FinishRun: %v", err)
			}
			got, _ := st.Run(run.ID)
			if got.Status != tc.want {
				t.Errorf("status = %s, want %s", got.Status, tc.want)
			}
		})
	}
}

func TestRunUnknownID(t *testing.T) {
	st, _ := openTestStore(t)
	got, err := st.Run("run-999")
	if err != nil || got != nil {
		t.Errorf("Run(run-999) = %v, %v; want nil, nil", got, err)
	}
	if _, err := st.Run("not-a-run"); err == nil {
		t.Error("Run(not-a-run) should report an invalid id")
	}
}

func TestListRunsNewestFirstWithLimit(t *testing.T) {
	st, _ := openTestStore(t)
	inst := &Instance{ID: "i", Script: "hello", RunnerID: "h"}
	for i := 0; i < 3; i++ {
		if _, err := st.CreateRun(inst); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
	}
	all, err := st.ListRuns(0)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(all) != 3 || all[0].ID != "run-3" {
		t.Errorf("ListRuns(0) = %d runs, first %s; want 3, run-3", len(all), all[0].ID)
	}
	limited, err := st.ListRuns(2)
	if err != nil {
		t.Fatalf("ListRuns(2): %v", err)
	}
	if len(limited) != 2 || limited[0].ID != "run-3" {
		t.Errorf("ListRuns(2) = %d runs, first %s; want 2, run-3", len(limited), limited[0].ID)
	}
}

func TestRecordCheckinUpsertsRunner(t *testing.T) {
	st, _ := openTestStore(t)
	first := time.Now().Add(-time.Hour)
	req := proto.CheckinRequest{RunnerID: "host-1", Hostname: "host-1", OS: "linux", Arch: "amd64"}
	if err := st.RecordCheckin(req, first); err != nil {
		t.Fatalf("RecordCheckin: %v", err)
	}
	later := time.Now()
	if err := st.RecordCheckin(req, later); err != nil {
		t.Fatalf("RecordCheckin again: %v", err)
	}
	runners, err := st.Runners()
	if err != nil {
		t.Fatalf("Runners: %v", err)
	}
	if len(runners) != 1 {
		t.Fatalf("got %d runners, want 1 (upsert)", len(runners))
	}
	r := runners[0]
	if r.Arch != "amd64" || r.OS != "linux" {
		t.Errorf("runner platform = %s/%s, want linux/amd64", r.OS, r.Arch)
	}
	if !r.FirstSeen.Equal(first.UTC().Truncate(time.Millisecond)) {
		t.Errorf("first_seen = %s, want preserved %s", r.FirstSeen, first.UTC())
	}
	if !r.LastSeen.After(r.FirstSeen) {
		t.Errorf("last_seen %s should be after first_seen %s", r.LastSeen, r.FirstSeen)
	}
}

func TestAppendOutputAndStreamPath(t *testing.T) {
	st, _ := openTestStore(t)
	if n, err := st.AppendOutput("run-1", "stdout", []byte("hello ")); err != nil || n != 6 {
		t.Fatalf("AppendOutput = %d, %v; want 6, nil", n, err)
	}
	if _, err := st.AppendOutput("run-1", "stdout", []byte("world")); err != nil {
		t.Fatalf("AppendOutput: %v", err)
	}
	data, err := os.ReadFile(st.OutputPath("run-1"))
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(data) != "hello world" {
		t.Errorf("output = %q, want %q (appended)", data, "hello world")
	}
	// An unknown stream name must not escape into a stray file.
	if got := st.StreamPath("run-1", "../../etc/passwd"); filepath.Base(got) != "stdout.log" {
		t.Errorf("StreamPath fallback = %s, want stdout.log", got)
	}
	if got := st.StreamPath("run-1", "stderr"); filepath.Base(got) != "stderr.log" {
		t.Errorf("StreamPath(stderr) = %s, want stderr.log", got)
	}
}

// Data written by one Store must be visible to a Store reopened on the same file —
// this is the point of moving off the in-memory store.
func TestPersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	backupDir := filepath.Join(dir, "backup")

	st, err := Open(dbPath, backupDir, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	inst := &Instance{ID: "hello-demo", Script: "hello", RunnerID: "host-1"}
	path := writeInstances(t, dir, []*Instance{inst})
	if _, err := st.ImportInstances(path); err != nil {
		t.Fatalf("import: %v", err)
	}
	run, err := st.CreateRun(inst)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := st.FinishRun(run.ID, time.Now(), 0, ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open(dbPath, backupDir, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	gotInst, err := reopened.Instance("hello-demo")
	if err != nil || gotInst == nil {
		t.Fatalf("instance lost across reopen: %v", err)
	}
	runs, err := reopened.ListRuns(0)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].Status != StatusSuccess {
		t.Fatalf("runs lost across reopen: %+v", runs)
	}
	// Run ids must keep counting up, not restart at 1 and collide.
	next, err := reopened.CreateRun(inst)
	if err != nil {
		t.Fatalf("CreateRun after reopen: %v", err)
	}
	if next.ID != "run-2" {
		t.Errorf("run id after reopen = %s, want run-2", next.ID)
	}
}

// Secret values must never leave the server except inside a dispatch to the owning
// runner — the API and logs get names only.
func TestInstanceRedactedMasksSecretValues(t *testing.T) {
	in := &Instance{
		ID:      "mysql-web01",
		Params:  map[string]string{"host": "127.0.0.1"},
		Secrets: map[string]string{"password": "hunter2", "token": "abc"},
	}
	red := in.Redacted()
	for k, v := range red.Secrets {
		if v != "***" {
			t.Errorf("secret %q = %q, want masked", k, v)
		}
	}
	if len(red.Secrets) != 2 {
		t.Errorf("secret names lost: %v", red.Secrets)
	}
	if red.Params["host"] != "127.0.0.1" {
		t.Error("non-secret params must stay intact")
	}
	// The original must be untouched, or dispatch would ship masked secrets.
	if in.Secrets["password"] != "hunter2" {
		t.Errorf("Redacted mutated the original: %v", in.Secrets)
	}
}
