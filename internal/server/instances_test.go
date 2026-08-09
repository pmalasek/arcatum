package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"arcatum/pkg/crypto"
	"arcatum/pkg/jobspec"
	"arcatum/pkg/proto"
)

// instanceAPIServer builds a server with a script catalogue an instance can refer to.
func instanceAPIServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	keyPath := masterKeyFile(t, dir, "master.key")
	kr, err := crypto.LoadKeyring(keyPath, nil)
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	st, err := Open(filepath.Join(dir, "test.db"), filepath.Join(dir, "backup"), kr)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	catalog := &Catalog{byName: map[string]*ScriptEntry{
		"mysql-backup": {Manifest: &jobspec.Manifest{
			Name: "mysql-backup", Type: proto.TypeBash, Entrypoint: "mysql.sh",
			Params: []jobspec.Param{
				{Name: "host", Type: "string", Required: true},
				{Name: "port", Type: "int", Default: "3306"},
				{Name: "database", Type: "string", Required: true},
				{Name: "password", Type: "string", Required: true, Secret: true},
			},
		}},
		"files-backup": {Manifest: &jobspec.Manifest{
			Name: "files-backup", Type: proto.TypeRestic,
			Params: []jobspec.Param{
				{Name: "paths", Type: "string", Required: true},
				{Name: "restic_password", Type: "string", Required: true, Secret: true, Default: "password"},
			},
		}},
	}}
	return &Server{
		store:   st,
		log:     log.New(io.Discard, "", 0),
		sched:   NewScheduler(time.UTC),
		catalog: catalog,
	}
}

// apiCall performs a JSON request against the mux and returns the recorder.
func apiCall(t *testing.T, srv *Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	r := httptest.NewRequest(method, path, reader)
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	return rec
}

func validInstance() map[string]any {
	return map[string]any{
		"id":        "mysql-web01",
		"script":    "mysql-backup",
		"runner_id": "web-01",
		"params":    map[string]string{"host": "127.0.0.1", "database": "shop"},
		"secrets":   map[string]string{"password": "hunter2"},
	}
}

// Creating an instance through the API is what replaces editing JSON on the server: the
// secret is encrypted from the moment it is stored. When it runs is a separate decision,
// made under Schedules — a new task is runnable on demand and on no timetable.
func TestCreateInstance(t *testing.T) {
	srv := instanceAPIServer(t)

	rec := apiCall(t, srv, http.MethodPost, "/api/v1/instances", validInstance())
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	// The response must not echo the secret back.
	if strings.Contains(rec.Body.String(), "hunter2") {
		t.Error("the response leaked the secret value")
	}

	stored, err := srv.store.Instance("mysql-web01")
	if err != nil || stored == nil {
		t.Fatalf("Instance: %v", err)
	}
	if stored.Secrets["password"] != "hunter2" {
		t.Errorf("secret round-trip failed: %q", stored.Secrets["password"])
	}
	// No schedule comes with it: the two are created separately now.
	schedules, err := srv.store.SchedulesForInstance("mysql-web01")
	if err != nil || len(schedules) != 0 {
		t.Errorf("SchedulesForInstance = %v, %v; a new instance starts with none", schedules, err)
	}
	// It can still be run by hand, which is the whole point of an instance with no timetable.
	if rec := apiCall(t, srv, http.MethodPost, "/api/v1/instances/mysql-web01/run", nil); rec.Code != http.StatusOK {
		t.Errorf("run now = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

// A declared default has to end up in the stored instance, not just satisfy validation:
// the dispatch carries what is stored, so a value left only in the manifest would have the
// job fail on the runner for a parameter the API accepted.
func TestCreateInstanceAppliesDefaults(t *testing.T) {
	srv := instanceAPIServer(t)

	payload := map[string]any{
		"id":        "files-web01",
		"script":    "files-backup",
		"runner_id": "web-01",
		"params":    map[string]string{"paths": "/etc"},
		"secrets":   map[string]string{}, // restic_password left out entirely
	}
	if rec := apiCall(t, srv, http.MethodPost, "/api/v1/instances", payload); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	stored, err := srv.store.Instance("files-web01")
	if err != nil || stored == nil {
		t.Fatalf("Instance: %v", err)
	}
	if stored.Secrets["restic_password"] != "password" {
		t.Errorf("restic_password = %q, want the default %q", stored.Secrets["restic_password"], "password")
	}

	// A blank field means "not supplied" too, and a default for a plain parameter works
	// the same way as one for a secret.
	payload["id"] = "files-web02"
	payload["secrets"] = map[string]string{"restic_password": ""}
	if rec := apiCall(t, srv, http.MethodPost, "/api/v1/instances", payload); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	if stored, _ = srv.store.Instance("files-web02"); stored.Secrets["restic_password"] != "password" {
		t.Errorf("blank restic_password = %q, want the default", stored.Secrets["restic_password"])
	}

	mysql := validInstance()
	if rec := apiCall(t, srv, http.MethodPost, "/api/v1/instances", mysql); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", rec.Code, rec.Body.String())
	}
	if stored, _ = srv.store.Instance("mysql-web01"); stored.Params["port"] != "3306" {
		t.Errorf("port = %q, want the default 3306", stored.Params["port"])
	}
}

// A supplied value always wins over the default — otherwise a real repository password
// could be quietly replaced by the placeholder.
func TestCreateInstanceKeepsSuppliedValueOverDefault(t *testing.T) {
	srv := instanceAPIServer(t)

	payload := map[string]any{
		"id":        "files-web01",
		"script":    "files-backup",
		"runner_id": "web-01",
		"params":    map[string]string{"paths": "/etc"},
		"secrets":   map[string]string{"restic_password": "a-real-one"},
	}
	if rec := apiCall(t, srv, http.MethodPost, "/api/v1/instances", payload); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	stored, err := srv.store.Instance("files-web01")
	if err != nil || stored == nil {
		t.Fatalf("Instance: %v", err)
	}
	if stored.Secrets["restic_password"] != "a-real-one" {
		t.Errorf("restic_password = %q, want the supplied value", stored.Secrets["restic_password"])
	}
}

func TestCreateInstanceRejectsDuplicate(t *testing.T) {
	srv := instanceAPIServer(t)
	if rec := apiCall(t, srv, http.MethodPost, "/api/v1/instances", validInstance()); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d", rec.Code)
	}
	rec := apiCall(t, srv, http.MethodPost, "/api/v1/instances", validInstance())
	if rec.Code != http.StatusConflict {
		t.Errorf("duplicate create = %d, want 409", rec.Code)
	}
}

// Validation against the manifest is the point of declaring parameters: mistakes surface
// on save, not when the backup runs.
func TestCreateInstanceValidation(t *testing.T) {
	srv := instanceAPIServer(t)

	tests := []struct {
		name    string
		mutate  func(map[string]any)
		wantMsg string
	}{
		{"missing required param", func(m map[string]any) {
			m["params"] = map[string]string{"host": "127.0.0.1"} // no database
		}, "database"},
		{"missing required secret", func(m map[string]any) {
			m["secrets"] = map[string]string{}
		}, "password"},
		{"typo in parameter name", func(m map[string]any) {
			m["params"] = map[string]string{"host": "h", "database": "d", "datbase": "oops"}
		}, "datbase"},
		{"secret sent as a plain parameter", func(m map[string]any) {
			p := m["params"].(map[string]string)
			p["password"] = "hunter2"
		}, "secret"},
		{"non-numeric int", func(m map[string]any) {
			p := m["params"].(map[string]string)
			p["port"] = "not-a-number"
		}, "port"},
		{"unknown script", func(m map[string]any) { m["script"] = "nope" }, "unknown script"},
		{"no runner", func(m map[string]any) { m["runner_id"] = "" }, "runner_id"},
		{"bad id", func(m map[string]any) { m["id"] = "../etc/passwd" }, "invalid instance id"},
		{"bad timeout", func(m map[string]any) { m["timeout"] = "later" }, "timeout"},
		{"bad capture", func(m map[string]any) { m["capture"] = "maybe" }, "capture"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload := validInstance()
			tc.mutate(payload)
			rec := apiCall(t, srv, http.MethodPost, "/api/v1/instances", payload)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("create = %d, want 400 (%s)", rec.Code, rec.Body.String())
			}
			// The reason has to arrive as {"error": ...}: that is the only shape the web
			// UI reads, and a plain-text body leaves the operator staring at "400".
			var body struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("error body is not JSON (%s): %v", rec.Body.String(), err)
			}
			if !strings.Contains(body.Error, tc.wantMsg) {
				t.Errorf("error %q should mention %q", body.Error, tc.wantMsg)
			}
		})
	}
}

// The API hands out secrets masked. Saving a form back must therefore keep the stored
// value, not overwrite every password with "***".
func TestUpdateInstanceKeepsMaskedSecret(t *testing.T) {
	srv := instanceAPIServer(t)
	if rec := apiCall(t, srv, http.MethodPost, "/api/v1/instances", validInstance()); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d", rec.Code)
	}

	// Exactly what a form round-trip sends back.
	payload := validInstance()
	payload["secrets"] = map[string]string{"password": redactedSecret}
	payload["params"] = map[string]string{"host": "10.0.0.5", "database": "orders"}
	rec := apiCall(t, srv, http.MethodPut, "/api/v1/instances/mysql-web01", payload)
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	stored, err := srv.store.Instance("mysql-web01")
	if err != nil || stored == nil {
		t.Fatalf("Instance: %v", err)
	}
	if stored.Secrets["password"] != "hunter2" {
		t.Errorf("password = %q, want the stored value kept", stored.Secrets["password"])
	}
	if stored.Params["host"] != "10.0.0.5" || stored.Params["database"] != "orders" {
		t.Errorf("params not updated: %+v", stored.Params)
	}
}

// A secret the payload does not mention at all must also survive.
func TestUpdateInstanceKeepsOmittedSecret(t *testing.T) {
	srv := instanceAPIServer(t)
	apiCall(t, srv, http.MethodPost, "/api/v1/instances", validInstance())

	payload := validInstance()
	delete(payload, "secrets")
	if rec := apiCall(t, srv, http.MethodPut, "/api/v1/instances/mysql-web01", payload); rec.Code != http.StatusOK {
		t.Fatalf("update = %d (%s)", rec.Code, rec.Body.String())
	}
	stored, _ := srv.store.Instance("mysql-web01")
	if stored.Secrets["password"] != "hunter2" {
		t.Errorf("password = %q, want it kept", stored.Secrets["password"])
	}
}

func TestUpdateInstanceChangesSecret(t *testing.T) {
	srv := instanceAPIServer(t)
	apiCall(t, srv, http.MethodPost, "/api/v1/instances", validInstance())

	payload := validInstance()
	payload["secrets"] = map[string]string{"password": "new-password"}
	if rec := apiCall(t, srv, http.MethodPut, "/api/v1/instances/mysql-web01", payload); rec.Code != http.StatusOK {
		t.Fatalf("update = %d", rec.Code)
	}
	stored, _ := srv.store.Instance("mysql-web01")
	if stored.Secrets["password"] != "new-password" {
		t.Errorf("password = %q, want the new value", stored.Secrets["password"])
	}
}

// Copying an instance is how a second database on the same server gets configured: the
// form can only send secrets back masked, so the copy has to pick them up from the source
// rather than make the operator retype every password.
func TestCreateInstanceCopyFrom(t *testing.T) {
	srv := instanceAPIServer(t)
	if rec := apiCall(t, srv, http.MethodPost, "/api/v1/instances", validInstance()); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d", rec.Code)
	}

	// Exactly what the form sends for a copy: the source's values, the secret masked, a
	// new id and the one parameter that actually differs.
	payload := validInstance()
	payload["id"] = "mysql-web01-orders"
	payload["copy_from"] = "mysql-web01"
	payload["secrets"] = map[string]string{"password": redactedSecret}
	payload["params"] = map[string]string{"host": "127.0.0.1", "database": "orders"}
	rec := apiCall(t, srv, http.MethodPost, "/api/v1/instances", payload)
	if rec.Code != http.StatusCreated {
		t.Fatalf("copy = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}

	copied, err := srv.store.Instance("mysql-web01-orders")
	if err != nil || copied == nil {
		t.Fatalf("Instance: %v", err)
	}
	if copied.Secrets["password"] != "hunter2" {
		t.Errorf("password = %q, want the source's value", copied.Secrets["password"])
	}
	if copied.Params["database"] != "orders" {
		t.Errorf("database = %q, want the value the copy was given", copied.Params["database"])
	}
	// The source is untouched.
	source, _ := srv.store.Instance("mysql-web01")
	if source.Params["database"] != "shop" {
		t.Errorf("source database = %q, want it left alone", source.Params["database"])
	}
}

// A copy may switch script, and the source's secrets then belong to parameters the new
// script does not declare. Dragging them along would fail validation on a name the
// operator never typed; the copy takes only what it asked for.
func TestCreateInstanceCopyToOtherScript(t *testing.T) {
	srv := instanceAPIServer(t)
	if rec := apiCall(t, srv, http.MethodPost, "/api/v1/instances", validInstance()); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d", rec.Code)
	}

	payload := map[string]any{
		"id":        "files-web01",
		"copy_from": "mysql-web01",
		"script":    "files-backup",
		"runner_id": "web-01",
		"params":    map[string]string{"paths": "/etc"},
		"secrets":   map[string]string{"restic_password": "a-real-one"},
	}
	rec := apiCall(t, srv, http.MethodPost, "/api/v1/instances", payload)
	if rec.Code != http.StatusCreated {
		t.Fatalf("copy = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	copied, _ := srv.store.Instance("files-web01")
	if _, ok := copied.Secrets["password"]; ok {
		t.Error("the copy kept a secret belonging to the source's script")
	}
	if copied.Secrets["restic_password"] != "a-real-one" {
		t.Errorf("restic_password = %q, want the supplied value", copied.Secrets["restic_password"])
	}
}

// A copy of something that is not there must say so, not create an instance with whatever
// secrets the payload happened to carry.
func TestCreateInstanceCopyFromUnknown(t *testing.T) {
	srv := instanceAPIServer(t)
	payload := validInstance()
	payload["copy_from"] = "nope"
	rec := apiCall(t, srv, http.MethodPost, "/api/v1/instances", payload)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("copy from unknown = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "nope") {
		t.Errorf("error %q should name the missing source", rec.Body.String())
	}
	if got, _ := srv.store.Instance("mysql-web01"); got != nil {
		t.Error("the instance was created despite the unknown source")
	}
}

func TestUpdateUnknownInstance(t *testing.T) {
	srv := instanceAPIServer(t)
	rec := apiCall(t, srv, http.MethodPut, "/api/v1/instances/nope", validInstance())
	if rec.Code != http.StatusNotFound {
		t.Errorf("update unknown = %d, want 404", rec.Code)
	}
}

// Deleting a configuration entry must not throw away the backups it produced — but it
// must take the schedules with it, or the scheduler would keep firing for a task that is
// no longer there.
func TestDeleteInstanceKeepsRepository(t *testing.T) {
	srv := instanceAPIServer(t)
	apiCall(t, srv, http.MethodPost, "/api/v1/instances", validInstance())
	seedSchedule(t, srv, "mysql-web01", "02:30")

	if rec := apiCall(t, srv, http.MethodDelete, "/api/v1/instances/mysql-web01", nil); rec.Code != http.StatusOK {
		t.Fatalf("delete = %d", rec.Code)
	}
	got, err := srv.store.Instance("mysql-web01")
	if err != nil || got != nil {
		t.Errorf("instance still present: %v", err)
	}
	schedules, err := srv.store.SchedulesForInstance("mysql-web01")
	if err != nil || len(schedules) != 0 {
		t.Errorf("SchedulesForInstance = %v, %v; the schedules must go with the instance", schedules, err)
	}
	// And it stops being scheduled.
	if _, ok := srv.sched.NextRunForInstance("mysql-web01"); ok {
		t.Error("a deleted instance must stop being scheduled")
	}
	if rec := apiCall(t, srv, http.MethodDelete, "/api/v1/instances/mysql-web01", nil); rec.Code != http.StatusNotFound {
		t.Errorf("deleting twice = %d, want 404", rec.Code)
	}
}

// The form is built from these declarations, so they must come through with the flags
// that decide how each field is rendered.
func TestListScripts(t *testing.T) {
	srv := instanceAPIServer(t)
	rec := apiCall(t, srv, http.MethodGet, "/api/v1/scripts", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("scripts = %d", rec.Code)
	}
	var out []ScriptInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d scripts, want 2", len(out))
	}
	var mysql *ScriptInfo
	for i := range out {
		if out[i].Name == "mysql-backup" {
			mysql = &out[i]
		}
	}
	if mysql == nil {
		t.Fatal("mysql-backup missing")
	}
	byName := map[string]ParamInfo{}
	for _, p := range mysql.Params {
		byName[p.Name] = p
	}
	if !byName["password"].Secret {
		t.Error("password must be flagged as a secret so the form masks it")
	}
	if !byName["host"].Required {
		t.Error("host must be flagged required")
	}
	if byName["port"].Default != "3306" {
		t.Errorf("port default = %q, want 3306", byName["port"].Default)
	}
}

// Instances are editable through the API, so re-importing the seed file on every start
// must not undo those edits.
func TestImportDoesNotClobberManagedInstances(t *testing.T) {
	srv := instanceAPIServer(t)
	apiCall(t, srv, http.MethodPost, "/api/v1/instances", validInstance())

	// The seed file still describes the original state.
	dir := t.TempDir()
	path := writeInstances(t, dir, []*seedInstance{{
		Instance: Instance{
			ID: "mysql-web01", Script: "mysql-backup", RunnerID: "SEEDED-RUNNER",
			Params:  map[string]string{"host": "seed", "database": "seed"},
			Secrets: map[string]string{"password": "seed"},
		},
		Schedules: []ScheduleJSON{{Frequency: "daily", Time: "01:00"}},
	}})

	n, err := srv.store.ImportInstances(path, false)
	if err != nil {
		t.Fatalf("ImportInstances: %v", err)
	}
	if n != 0 {
		t.Errorf("imported %d, want 0 — the instance is already managed", n)
	}
	stored, _ := srv.store.Instance("mysql-web01")
	if stored.RunnerID != "web-01" {
		t.Errorf("runner = %q, want the API-managed value kept", stored.RunnerID)
	}

	// With overwrite the operator can still force the seed in.
	n, err = srv.store.ImportInstances(path, true)
	if err != nil {
		t.Fatalf("ImportInstances(overwrite): %v", err)
	}
	if n != 1 {
		t.Errorf("imported %d, want 1 with overwrite", n)
	}
	stored, _ = srv.store.Instance("mysql-web01")
	if stored.RunnerID != "SEEDED-RUNNER" {
		t.Errorf("runner = %q, want the seed applied with overwrite", stored.RunnerID)
	}
}

func TestInstanceWriteEndpointsRequireAdmin(t *testing.T) {
	srv := instanceAPIServer(t)
	srv.requireClientCert = true

	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/instances"},
		{http.MethodPut, "/api/v1/instances/mysql-web01"},
		{http.MethodDelete, "/api/v1/instances/mysql-web01"},
		{http.MethodGet, "/api/v1/scripts"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
			r.TLS = requestWithCert(certWithCNAndRole("web-01", crypto.RoleRunner)).TLS
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, r)
			if rec.Code != http.StatusForbidden {
				t.Errorf("as runner = %d, want 403", rec.Code)
			}
		})
	}
}
