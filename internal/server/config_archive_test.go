package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"arcatum/pkg/crypto"
	"arcatum/pkg/jobspec"
	"arcatum/pkg/proto"
)

// configServer builds a server whose secrets are sealed with the master key at keyPath,
// so two of them can be given the same key — or deliberately different ones.
func configServer(t *testing.T, keyPath string) *Server {
	t.Helper()
	dir := t.TempDir()
	kr, err := crypto.LoadKeyring(keyPath, nil)
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	st, err := Open(filepath.Join(dir, "test.db"), filepath.Join(dir, "backup"), kr)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return &Server{
		store: st,
		log:   log.New(io.Discard, "", 0),
		sched: NewScheduler(time.UTC),
		catalog: &Catalog{byName: map[string]*ScriptEntry{
			"mysql-backup": {Manifest: &jobspec.Manifest{
				Name: "mysql-backup", Type: proto.TypeBash, Entrypoint: "mysql.sh",
				Params: []jobspec.Param{
					{Name: "host", Type: "string", Required: true},
					{Name: "password", Type: "string", Required: true, Secret: true},
				},
			}},
		}},
	}
}

// seedConfig fills a server with one of everything an archive carries.
func seedConfig(t *testing.T, srv *Server) {
	t.Helper()
	inst := &Instance{
		ID: "mysql-web01", Script: "mysql-backup", RunnerID: "web-01",
		Params:   map[string]string{"host": "127.0.0.1"},
		Secrets:  map[string]string{"password": "hunter2"},
		Schedule: ScheduleJSON{Frequency: "daily", Time: "02:30"},
	}
	if err := srv.store.SaveInstance(inst, true); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}
	if err := srv.sched.Track(inst, time.Now()); err != nil {
		t.Fatalf("Track: %v", err)
	}
	if _, err := srv.store.CreateUser("petr", "adminpassword", UserRoleAdmin); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := srv.store.CreateUser("hlidac", "viewerpassword", UserRoleViewer); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	now := time.Now()
	if err := srv.store.SubmitEnrollment("web-01", "web-01", "linux", "amd64", "CSR", "10.0.0.5", now); err != nil {
		t.Fatalf("SubmitEnrollment: %v", err)
	}
	if err := srv.store.ApproveEnrollment("web-01", "CERT-PEM", "ab:cd", now.Add(time.Hour), now); err != nil {
		t.Fatalf("ApproveEnrollment: %v", err)
	}
}

// exportArchive downloads the configuration through the HTTP handler.
func exportArchive(t *testing.T, srv *Server) []byte {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/config/export", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("export = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
		t.Errorf("Content-Type = %q, want application/zip", ct)
	}
	return rec.Body.Bytes()
}

// importArchive uploads an archive; query is appended to the path (e.g. the confirm
// parameter that turns a dry run into a real import).
func importArchive(t *testing.T, srv *Server, data []byte, query string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/config/import"+query, bytes.NewReader(data))
	r.Header.Set("Content-Type", "application/zip")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	return rec
}

func decodePlan(t *testing.T, rec *httptest.ResponseRecorder) *ConfigImportPlan {
	t.Helper()
	var plan ConfigImportPlan
	if err := json.Unmarshal(rec.Body.Bytes(), &plan); err != nil {
		t.Fatalf("decode plan: %v (%s)", err, rec.Body.String())
	}
	return &plan
}

// The point of the whole feature: what comes out of one server goes into another and
// the configuration is the same on the other side — secrets included, which only works
// because both hold the same master key.
func TestConfigArchiveRoundTrip(t *testing.T) {
	keyPath := masterKeyFile(t, t.TempDir(), "master.key")
	source := configServer(t, keyPath)
	seedConfig(t, source)

	target := configServer(t, keyPath)
	rec := importArchive(t, target, exportArchive(t, source), "?confirm="+configImportConfirm)
	if rec.Code != http.StatusOK {
		t.Fatalf("import = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if plan := decodePlan(t, rec); !plan.Applied {
		t.Error("import reported applied = false")
	}

	inst, err := target.store.Instance("mysql-web01")
	if err != nil || inst == nil {
		t.Fatalf("Instance: %v", err)
	}
	// The secret travelled as ciphertext and is readable again on the other side.
	if inst.Secrets["password"] != "hunter2" {
		t.Errorf("secret = %q, want hunter2", inst.Secrets["password"])
	}
	if inst.Params["host"] != "127.0.0.1" || inst.Schedule.Time != "02:30" {
		t.Errorf("instance did not survive the round trip: %+v", inst)
	}

	// The account keeps working, which means the password verifier travelled intact.
	if _, err := target.store.Authenticate("petr", "adminpassword"); err != nil {
		t.Errorf("Authenticate after import: %v", err)
	}
	runners, err := target.store.Runners()
	if err != nil || len(runners) != 1 {
		t.Fatalf("Runners = %v, %v", runners, err)
	}
	if runners[0].Status != EnrollApproved {
		t.Errorf("runner status = %q, want approved", runners[0].Status)
	}
	// A restored instance is scheduled without a restart.
	if _, ok := target.sched.NextRun("mysql-web01"); !ok {
		t.Error("the imported instance is not on the schedule")
	}
}

// An import replaces rather than merges: whatever the archive does not have is gone
// afterwards, including from the scheduler.
func TestConfigImportRemovesWhatIsNotInTheArchive(t *testing.T) {
	keyPath := masterKeyFile(t, t.TempDir(), "master.key")
	source := configServer(t, keyPath)
	seedConfig(t, source)
	archive := exportArchive(t, source)

	// A second instance and account exist only on the target.
	extra := &Instance{
		ID: "mysql-web02", Script: "mysql-backup", RunnerID: "web-02",
		Params:   map[string]string{"host": "10.0.0.9"},
		Secrets:  map[string]string{"password": "other"},
		Schedule: ScheduleJSON{Frequency: "daily", Time: "04:00"},
	}
	target := configServer(t, keyPath)
	seedConfig(t, target)
	if err := target.store.SaveInstance(extra, true); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}
	if err := target.sched.Track(extra, time.Now()); err != nil {
		t.Fatalf("Track: %v", err)
	}
	if _, err := target.store.CreateUser("navic", "somepassword", UserRoleViewer); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	rec := importArchive(t, target, archive, "?confirm="+configImportConfirm)
	if rec.Code != http.StatusOK {
		t.Fatalf("import = %d (%s)", rec.Code, rec.Body.String())
	}
	plan := decodePlan(t, rec)
	if got := plan.Instances.Removed; len(got) != 1 || got[0] != "mysql-web02" {
		t.Errorf("removed instances = %v, want [mysql-web02]", got)
	}
	if got := plan.Users.Removed; len(got) != 1 || got[0] != "navic" {
		t.Errorf("removed users = %v, want [navic]", got)
	}

	if inst, err := target.store.Instance("mysql-web02"); err != nil || inst != nil {
		t.Errorf("instance mysql-web02 survived the import: %v, %v", inst, err)
	}
	if u, err := target.store.User("navic"); err != nil || u != nil {
		t.Errorf("account navic survived the import: %v, %v", u, err)
	}
	// Left on the schedule it would keep dispatching jobs for an instance that no longer
	// exists — the failure this is here to catch.
	if _, ok := target.sched.NextRun("mysql-web02"); ok {
		t.Error("a removed instance is still scheduled")
	}
}

// Without the confirm parameter an import says what it would do and does nothing, so a
// POST that arrives by accident cannot replace a configuration.
func TestConfigImportWithoutConfirmChangesNothing(t *testing.T) {
	keyPath := masterKeyFile(t, t.TempDir(), "master.key")
	source := configServer(t, keyPath)
	seedConfig(t, source)

	target := configServer(t, keyPath)
	rec := importArchive(t, target, exportArchive(t, source), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("dry run = %d (%s)", rec.Code, rec.Body.String())
	}
	plan := decodePlan(t, rec)
	if plan.Applied {
		t.Error("a dry run reported itself as applied")
	}
	if len(plan.Instances.Added) != 1 || plan.Instances.Added[0] != "mysql-web01" {
		t.Errorf("added instances = %v, want [mysql-web01]", plan.Instances.Added)
	}
	if inst, err := target.store.Instance("mysql-web01"); err != nil || inst != nil {
		t.Errorf("the dry run wrote to the database: %v, %v", inst, err)
	}
	if n, err := target.store.UserCount(); err != nil || n != 0 {
		t.Errorf("UserCount = %d, %v; want 0 after a dry run", n, err)
	}
}

// The one mistake that cannot be undone through the web UI: an archive with nobody who
// may administer the system.
func TestConfigImportRefusesArchiveWithoutAdmin(t *testing.T) {
	keyPath := masterKeyFile(t, t.TempDir(), "master.key")
	srv := configServer(t, keyPath)
	set := &ConfigSet{Users: []ConfigUser{{
		Username: "hlidac", PassHash: passwordHash(t, "viewerpassword"), Role: UserRoleViewer,
	}}}
	rec := importArchive(t, srv, archiveOf(t, set), "?confirm="+configImportConfirm)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("import = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "administrator") {
		t.Errorf("the refusal does not explain itself: %s", rec.Body.String())
	}
}

// An instance whose script is missing would be accepted into the database and then stop
// the server from starting again, so it is refused while that is still recoverable.
func TestConfigImportRefusesUnknownScript(t *testing.T) {
	keyPath := masterKeyFile(t, t.TempDir(), "master.key")
	srv := configServer(t, keyPath)
	set := &ConfigSet{
		Instances: []ConfigInstance{{
			ID: "pg-web01", Script: "postgres-backup", RunnerID: "web-01",
			Schedule: ScheduleJSON{Frequency: "daily", Time: "02:00"},
		}},
		Users: []ConfigUser{{
			Username: "petr", PassHash: passwordHash(t, "adminpassword"), Role: UserRoleAdmin,
		}},
	}
	rec := importArchive(t, srv, archiveOf(t, set), "?confirm="+configImportConfirm)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("import = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "postgres-backup") {
		t.Errorf("the refusal does not name the missing script: %s", rec.Body.String())
	}
}

// Secrets travel as ciphertext and the key does not travel at all, so an import onto a
// server with a different master key must fail loudly rather than restore instances
// whose passwords nobody can read.
func TestConfigImportRefusesForeignMasterKey(t *testing.T) {
	dir := t.TempDir()
	source := configServer(t, masterKeyFile(t, dir, "one.key"))
	seedConfig(t, source)

	target := configServer(t, masterKeyFile(t, dir, "two.key"))
	rec := importArchive(t, target, exportArchive(t, source), "?confirm="+configImportConfirm)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("import = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "master key") {
		t.Errorf("the refusal does not mention the key: %s", rec.Body.String())
	}
	if inst, err := target.store.Instance("mysql-web01"); err != nil || inst != nil {
		t.Errorf("the refused import still wrote: %v, %v", inst, err)
	}
}

// A damaged or hand-edited archive is refused whole. Half-importing one would be worse
// than not importing it at all.
func TestConfigImportRefusesTamperedArchive(t *testing.T) {
	keyPath := masterKeyFile(t, t.TempDir(), "master.key")
	source := configServer(t, keyPath)
	seedConfig(t, source)
	archive := exportArchive(t, source)

	tampered := rewriteArchiveEntry(t, archive, configUsersName, func(data []byte) []byte {
		return bytes.Replace(data, []byte(`"petr"`), []byte(`"karel"`), 1)
	})
	target := configServer(t, keyPath)
	rec := importArchive(t, target, tampered, "?confirm="+configImportConfirm)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("import = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "checksum") {
		t.Errorf("the refusal does not mention the checksum: %s", rec.Body.String())
	}
}

// Importing the wrong file is recoverable because the server saves what it is about to
// replace, as an archive that can simply be imported back.
func TestConfigImportSavesTheOldConfigurationFirst(t *testing.T) {
	keyPath := masterKeyFile(t, t.TempDir(), "master.key")
	source := configServer(t, keyPath)
	seedConfig(t, source)

	target := configServer(t, keyPath)
	seedConfig(t, target)
	if _, err := target.store.CreateUser("navic", "somepassword", UserRoleViewer); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	rec := importArchive(t, target, exportArchive(t, source), "?confirm="+configImportConfirm)
	if rec.Code != http.StatusOK {
		t.Fatalf("import = %d (%s)", rec.Code, rec.Body.String())
	}
	backup := decodePlan(t, rec).Backup
	if backup == "" {
		t.Fatal("the import did not report a backup of the previous configuration")
	}
	data, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	arc, err := readConfigArchive(data)
	if err != nil {
		t.Fatalf("the saved backup is not a readable archive: %v", err)
	}
	// It holds what was replaced, not what replaced it.
	var found bool
	for _, u := range arc.Set.Users {
		if u.Username == "navic" {
			found = true
		}
	}
	if !found {
		t.Error("the saved backup does not contain the configuration it replaced")
	}
}

// Import must never touch runs: history is not configuration, and the files it points
// at are still on disk.
func TestConfigImportKeepsRunHistory(t *testing.T) {
	keyPath := masterKeyFile(t, t.TempDir(), "master.key")
	source := configServer(t, keyPath)
	seedConfig(t, source)

	target := configServer(t, keyPath)
	seedConfig(t, target)
	inst, err := target.store.Instance("mysql-web01")
	if err != nil || inst == nil {
		t.Fatalf("Instance: %v", err)
	}
	if _, err := target.store.CreateRun(inst, 60); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	if rec := importArchive(t, target, exportArchive(t, source), "?confirm="+configImportConfirm); rec.Code != http.StatusOK {
		t.Fatalf("import = %d (%s)", rec.Code, rec.Body.String())
	}
	runs, err := target.store.ListRuns(0)
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRuns = %d runs, %v; want 1", len(runs), err)
	}
}

// A runner that is approved here but absent from the archive loses its access. That is
// the correct outcome, and it is also the one nobody expects, so the plan says it out
// loud before anything is applied.
func TestConfigImportWarnsAboutRunnersLosingAccess(t *testing.T) {
	keyPath := masterKeyFile(t, t.TempDir(), "master.key")
	source := configServer(t, keyPath)
	seedConfig(t, source)
	archive := exportArchive(t, source)

	target := configServer(t, keyPath)
	seedConfig(t, target)
	now := time.Now()
	if err := target.store.SubmitEnrollment("web-09", "web-09", "linux", "amd64", "CSR", "10.0.0.9", now); err != nil {
		t.Fatalf("SubmitEnrollment: %v", err)
	}
	if err := target.store.ApproveEnrollment("web-09", "CERT", "ef:01", now.Add(time.Hour), now); err != nil {
		t.Fatalf("ApproveEnrollment: %v", err)
	}

	plan := decodePlan(t, importArchive(t, target, archive, ""))
	var warned bool
	for _, w := range plan.Warnings {
		if strings.Contains(w, "web-09") && strings.Contains(w, "enrol again") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no warning about web-09 losing access: %v", plan.Warnings)
	}
}

// A runner that only checked in since the export is the same runner. Reporting it as a
// change would bury the differences that matter in noise.
func TestConfigDiffIgnoresCheckinDetails(t *testing.T) {
	keyPath := masterKeyFile(t, t.TempDir(), "master.key")
	srv := configServer(t, keyPath)
	seedConfig(t, srv)
	archive := exportArchive(t, srv)

	if err := srv.store.RecordCheckin(proto.CheckinRequest{
		RunnerID: "web-01", Hostname: "web-01", OS: "linux", Arch: "amd64", Version: "2026.08.08",
	}, time.Now().Add(time.Hour), time.Now(), "Arcatum CA"); err != nil {
		t.Fatalf("RecordCheckin: %v", err)
	}

	plan := decodePlan(t, importArchive(t, srv, archive, ""))
	if len(plan.Runners.Changed) != 0 {
		t.Errorf("runners reported as changed after a check-in: %v", plan.Runners.Changed)
	}
	if plan.Runners.Unchanged != 1 {
		t.Errorf("unchanged runners = %d, want 1", plan.Runners.Unchanged)
	}
}

// server.toml rides along for reference. Applying it would let an archive rewrite the
// listen address and lock everybody out of the machine.
func TestConfigArchiveCarriesButDoesNotApplyServerTOML(t *testing.T) {
	keyPath := masterKeyFile(t, t.TempDir(), "master.key")
	srv := configServer(t, keyPath)
	seedConfig(t, srv)
	cfgPath := filepath.Join(t.TempDir(), "server.toml")
	if err := os.WriteFile(cfgPath, []byte("[server]\nlisten = \"0.0.0.0:8443\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	srv.configPath = cfgPath

	arc, err := readConfigArchive(exportArchive(t, srv))
	if err != nil {
		t.Fatalf("readConfigArchive: %v", err)
	}
	if !strings.Contains(string(arc.ServerTOML), "0.0.0.0:8443") {
		t.Errorf("server.toml is not in the archive: %q", arc.ServerTOML)
	}

	target := configServer(t, keyPath)
	target.configPath = filepath.Join(t.TempDir(), "other.toml")
	if err := os.WriteFile(target.configPath, []byte("[server]\nlisten = \"127.0.0.1:9999\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if rec := importArchive(t, target, exportArchive(t, srv), "?confirm="+configImportConfirm); rec.Code != http.StatusOK {
		t.Fatalf("import = %d (%s)", rec.Code, rec.Body.String())
	}
	after, err := os.ReadFile(target.configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(after), "127.0.0.1:9999") {
		t.Errorf("the import overwrote server.toml: %q", after)
	}
}

// An empty or unrecognisable body should say what is wrong with it, not 500.
func TestConfigImportRejectsRubbish(t *testing.T) {
	srv := configServer(t, masterKeyFile(t, t.TempDir(), "master.key"))
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"empty", nil},
		{"not a zip", []byte("tohle rozhodně není zip")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rec := importArchive(t, srv, tc.body, ""); rec.Code != http.StatusBadRequest {
				t.Errorf("import = %d, want 400 (%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// --- helpers ------------------------------------------------------------------

func passwordHash(t *testing.T, password string) string {
	t.Helper()
	hash, err := crypto.HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	return hash
}

// archiveOf builds an archive around a hand-written configuration, for the cases where
// exporting one would mean first creating the very state being tested as invalid.
func archiveOf(t *testing.T, set *ConfigSet) []byte {
	t.Helper()
	data, err := buildConfigArchive(set, "test", "", time.Now())
	if err != nil {
		t.Fatalf("buildConfigArchive: %v", err)
	}
	return data
}

// rewriteArchiveEntry rebuilds an archive with one entry replaced, leaving the manifest
// — and therefore its checksums — untouched.
func rewriteArchiveEntry(t *testing.T, archive []byte, name string, edit func([]byte) []byte) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %s: %v", f.Name, err)
		}
		content, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read %s: %v", f.Name, err)
		}
		if f.Name == name {
			content = edit(content)
		}
		w, err := zw.Create(f.Name)
		if err != nil {
			t.Fatalf("create %s: %v", f.Name, err)
		}
		if _, err := w.Write(content); err != nil {
			t.Fatalf("write %s: %v", f.Name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close archive: %v", err)
	}
	return buf.Bytes()
}
