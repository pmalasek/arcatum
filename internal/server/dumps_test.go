package server

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func dumpServer(t *testing.T) *Server {
	t.Helper()
	st, _ := openTestStore(t)
	return &Server{store: st, log: log.New(io.Discard, "", 0)}
}

// storeDump records a finished run of instanceID with a payload of the given size,
// ended at the given time.
func storeDump(t *testing.T, st *Store, instanceID string, endedAt time.Time, size int) *Run {
	t.Helper()
	run, err := st.CreateRun(&Instance{ID: instanceID, Script: "mysql-backup", RunnerID: "db-01"}, 3600)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	f, err := st.CreateData(run.ID)
	if err != nil {
		t.Fatalf("CreateData: %v", err)
	}
	if _, err := f.Write(make([]byte, size)); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	f.Close()
	if err := st.SetRunDataBytes(run.ID, int64(size)); err != nil {
		t.Fatalf("SetRunDataBytes: %v", err)
	}
	if err := st.FinishRun(run.ID, endedAt, 0, ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	return run
}

// The core of it: keep the last N, drop the rest.
func TestDumpRetentionKeepsTheLastN(t *testing.T) {
	srv := dumpServer(t)
	now := time.Now()
	var runs []*Run
	for i := 0; i < 5; i++ {
		runs = append(runs, storeDump(t, srv.store, "db", now.Add(-time.Duration(5-i)*24*time.Hour), 16))
	}
	if err := srv.store.SaveInstance(&Instance{
		ID: "db", Script: "mysql-backup", RunnerID: "db-01", KeepLast: 2,
	}, true); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}

	srv.sweepDumps(now)

	// The two newest survive; the three older ones are gone.
	for i, run := range runs {
		_, err := os.Stat(srv.store.DataPath(run.ID))
		keep := i >= 3
		if keep && err != nil {
			t.Errorf("dump %d (%s) was removed but is among the last 2: %v", i, run.ID, err)
		}
		if !keep && !os.IsNotExist(err) {
			t.Errorf("dump %d (%s) survived retention (err %v)", i, run.ID, err)
		}
	}
	// The runs themselves stay, marked so the history still says what they produced.
	got, err := srv.store.Run(runs[0].ID)
	if err != nil || got == nil {
		t.Fatalf("Run: %v", err)
	}
	if !got.DataPruned {
		t.Error("pruned run is not marked, so the UI cannot tell it apart from one that produced nothing")
	}
	if got.DataBytes == 0 {
		t.Error("data_bytes was cleared; the history should still say what the run produced")
	}
}

// Zero for both means keep everything. Deleting a backup must never be a default
// somebody inherits without having asked for it.
func TestDumpRetentionDisabledKeepsEverything(t *testing.T) {
	srv := dumpServer(t)
	now := time.Now()
	old := storeDump(t, srv.store, "db", now.Add(-5*365*24*time.Hour), 16)
	if err := srv.store.SaveInstance(&Instance{ID: "db", Script: "mysql-backup", RunnerID: "db-01"}, true); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}

	srv.sweepDumps(now)

	if _, err := os.Stat(srv.store.DataPath(old.ID)); err != nil {
		t.Errorf("retention is off but a five-year-old dump was removed: %v", err)
	}
}

// keep_days on its own: age decides, however many there are.
func TestDumpRetentionByAge(t *testing.T) {
	srv := dumpServer(t)
	now := time.Now()
	recent := storeDump(t, srv.store, "db", now.Add(-2*24*time.Hour), 16)
	old := storeDump(t, srv.store, "db", now.Add(-40*24*time.Hour), 16)
	if err := srv.store.SaveInstance(&Instance{
		ID: "db", Script: "mysql-backup", RunnerID: "db-01", KeepDays: 30,
	}, true); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}

	srv.sweepDumps(now)

	if _, err := os.Stat(srv.store.DataPath(recent.ID)); err != nil {
		t.Errorf("a two-day-old dump was removed under a 30-day policy: %v", err)
	}
	if _, err := os.Stat(srv.store.DataPath(old.ID)); !os.IsNotExist(err) {
		t.Errorf("a 40-day-old dump survived a 30-day policy (err %v)", err)
	}
}

// The two are a union: "at least this many, and at least this old". A count too small
// for a quiet week must not shorten the window, and vice versa.
func TestDumpRetentionCombinesCountAndAge(t *testing.T) {
	srv := dumpServer(t)
	now := time.Now()
	// Three dumps, all older than the age window. keep_last must still hold two back.
	var runs []*Run
	for i := 0; i < 3; i++ {
		runs = append(runs, storeDump(t, srv.store, "db", now.Add(-time.Duration(40+3-i)*24*time.Hour), 16))
	}
	if err := srv.store.SaveInstance(&Instance{
		ID: "db", Script: "mysql-backup", RunnerID: "db-01", KeepLast: 2, KeepDays: 30,
	}, true); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}

	srv.sweepDumps(now)

	if _, err := os.Stat(srv.store.DataPath(runs[0].ID)); !os.IsNotExist(err) {
		t.Errorf("the oldest of three survived keep_last=2 (err %v)", err)
	}
	for _, run := range runs[1:] {
		if _, err := os.Stat(srv.store.DataPath(run.ID)); err != nil {
			t.Errorf("keep_last=2 did not hold back %s despite the age window: %v", run.ID, err)
		}
	}
}

// One instance's policy must never reach into another's dumps.
func TestDumpRetentionIsPerInstance(t *testing.T) {
	srv := dumpServer(t)
	now := time.Now()
	mine := storeDump(t, srv.store, "db", now.Add(-10*24*time.Hour), 16)
	theirs := storeDump(t, srv.store, "other", now.Add(-10*24*time.Hour), 16)
	for _, in := range []*Instance{
		{ID: "db", Script: "mysql-backup", RunnerID: "db-01", KeepLast: 1},
		{ID: "other", Script: "mysql-backup", RunnerID: "db-02"},
	} {
		if err := srv.store.SaveInstance(in, true); err != nil {
			t.Fatalf("SaveInstance: %v", err)
		}
	}
	// One more for "db" so its single kept slot is taken by the newer one.
	storeDump(t, srv.store, "db", now, 16)

	srv.sweepDumps(now)

	if _, err := os.Stat(srv.store.DataPath(mine.ID)); !os.IsNotExist(err) {
		t.Errorf("db's older dump survived keep_last=1 (err %v)", err)
	}
	if _, err := os.Stat(srv.store.DataPath(theirs.ID)); err != nil {
		t.Errorf("another instance's dump was removed by db's policy: %v", err)
	}
}

// A pruned dump has to read as "rotated away", not as "this run produced nothing".
func TestDownloadOfAPrunedDumpSaysSo(t *testing.T) {
	srv, _ := resticTestServer(t, false)
	run := startedRun(t, srv)
	if rec := uploadData(srv, run.ID, "a dump", nil); rec.Code != http.StatusOK {
		t.Fatalf("upload = %d, want 200", rec.Code)
	}
	if err := srv.store.FinishRun(run.ID, time.Now(), 0, ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	if _, err := srv.store.DeleteRunPayload(run.ID); err != nil {
		t.Fatalf("DeleteRunPayload: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+run.ID+"/data", nil))
	if rec.Code != http.StatusGone {
		t.Errorf("download of a pruned dump = %d, want 410", rec.Code)
	}
}

// The instance list must show a streaming instance's dumps, or one holding every backup
// it was told to keep reads as holding none.
func TestRepoInfoReportsDumps(t *testing.T) {
	srv, _ := resticTestServer(t, false)
	run := startedRun(t, srv) // instance files-web01
	if rec := uploadData(srv, run.ID, "a dump", nil); rec.Code != http.StatusOK {
		t.Fatalf("upload = %d, want 200", rec.Code)
	}
	if err := srv.store.FinishRun(run.ID, time.Now(), 0, ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/instances/files-web01/repo", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("repo info = %d, want 200", rec.Code)
	}
	var info ResticRepoInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if info.Exists {
		t.Error("there is no restic repository for this instance")
	}
	if info.Dumps.Count != 1 || info.Dumps.Bytes != int64(len("a dump")) {
		t.Errorf("dumps = %+v, want 1 dump of %d bytes", info.Dumps, len("a dump"))
	}
	if info.Dumps.Last.IsZero() {
		t.Error("no timestamp for the newest dump")
	}
}

// The restore view lists dumps the way it lists snapshots for a file backup.
func TestListDumpsEndpoint(t *testing.T) {
	srv, _ := resticTestServer(t, false)
	run := startedRun(t, srv)
	if rec := uploadData(srv, run.ID, "a dump", nil); rec.Code != http.StatusOK {
		t.Fatalf("upload = %d, want 200", rec.Code)
	}
	if err := srv.store.FinishRun(run.ID, time.Now(), 0, ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/instances/files-web01/dumps", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("dumps = %d, want 200", rec.Code)
	}
	var dumps []DumpInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &dumps); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(dumps) != 1 || dumps[0].RunID != run.ID {
		t.Fatalf("dumps = %+v, want one entry for %s", dumps, run.ID)
	}
}

// A failed run has no dump — the payload was discarded — so it must not be listed as one.
func TestFailedRunIsNotListedAsADump(t *testing.T) {
	srv, _ := resticTestServer(t, false)
	run := startedRun(t, srv)
	if rec := uploadData(srv, run.ID, "half a dump", nil); rec.Code != http.StatusOK {
		t.Fatalf("upload = %d, want 200", rec.Code)
	}
	if err := srv.store.FinishRun(run.ID, time.Now(), 1, ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	dumps, err := srv.store.InstanceDumps("files-web01", 0)
	if err != nil {
		t.Fatalf("InstanceDumps: %v", err)
	}
	if len(dumps) != 0 {
		t.Errorf("dumps = %+v, want none — the run failed and its payload was discarded", dumps)
	}
}
