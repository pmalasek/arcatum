package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The live tail is offset-based: each poll asks for output from where the last one
// stopped, so nothing is re-sent and nothing is skipped.
func TestReadOutputFromOffset(t *testing.T) {
	st, _ := openTestStore(t)
	if _, err := st.AppendOutput("run-1", "stdout", []byte("hello ")); err != nil {
		t.Fatalf("AppendOutput: %v", err)
	}

	data, off, err := st.ReadOutputFrom("run-1", "stdout", 0, 1024)
	if err != nil {
		t.Fatalf("ReadOutputFrom: %v", err)
	}
	if string(data) != "hello " || off != 6 {
		t.Fatalf("first read = %q/%d, want %q/6", data, off, "hello ")
	}

	// A poll with nothing new must return no data and the same offset.
	data, off2, err := st.ReadOutputFrom("run-1", "stdout", off, 1024)
	if err != nil {
		t.Fatalf("ReadOutputFrom: %v", err)
	}
	if len(data) != 0 || off2 != off {
		t.Errorf("idle poll = %q/%d, want empty/%d", data, off2, off)
	}

	// Only the newly appended bytes come back.
	if _, err := st.AppendOutput("run-1", "stdout", []byte("world")); err != nil {
		t.Fatalf("AppendOutput: %v", err)
	}
	data, off3, err := st.ReadOutputFrom("run-1", "stdout", off2, 1024)
	if err != nil {
		t.Fatalf("ReadOutputFrom: %v", err)
	}
	if string(data) != "world" || off3 != 11 {
		t.Errorf("incremental read = %q/%d, want %q/11", data, off3, "world")
	}
}

func TestReadOutputFromEdgeCases(t *testing.T) {
	st, _ := openTestStore(t)

	// No output captured yet is not an error — the run may not have started.
	data, off, err := st.ReadOutputFrom("run-404", "stdout", 0, 1024)
	if err != nil || len(data) != 0 || off != 0 {
		t.Errorf("missing file = %q/%d/%v, want empty/0/nil", data, off, err)
	}

	if _, err := st.AppendOutput("run-1", "stdout", []byte("0123456789")); err != nil {
		t.Fatalf("AppendOutput: %v", err)
	}

	// max caps a single response so one poll cannot pull an enormous log.
	data, off, err = st.ReadOutputFrom("run-1", "stdout", 0, 4)
	if err != nil {
		t.Fatalf("ReadOutputFrom: %v", err)
	}
	if string(data) != "0123" || off != 4 {
		t.Errorf("capped read = %q/%d, want 0123/4", data, off)
	}

	// An offset past the end (file truncated or replaced) restarts from the beginning
	// rather than returning garbage.
	data, off, err = st.ReadOutputFrom("run-1", "stdout", 999, 1024)
	if err != nil {
		t.Fatalf("ReadOutputFrom: %v", err)
	}
	if string(data) != "0123456789" || off != 10 {
		t.Errorf("over-long offset = %q/%d, want the whole file", data, off)
	}
}

func TestReadOutputFromSeparatesStreams(t *testing.T) {
	st, _ := openTestStore(t)
	if _, err := st.AppendOutput("run-1", "stdout", []byte("out")); err != nil {
		t.Fatalf("AppendOutput: %v", err)
	}
	if _, err := st.AppendOutput("run-1", "stderr", []byte("err")); err != nil {
		t.Fatalf("AppendOutput: %v", err)
	}
	out, _, _ := st.ReadOutputFrom("run-1", "stdout", 0, 1024)
	errOut, _, _ := st.ReadOutputFrom("run-1", "stderr", 0, 1024)
	if string(out) != "out" || string(errOut) != "err" {
		t.Errorf("streams crossed: stdout=%q stderr=%q", out, errOut)
	}
}

// tailFor performs one tail request against the handler.
func tailFor(t *testing.T, srv *Server, runID string, offset int64, stream string) tailResponse {
	t.Helper()
	url := "/api/v1/runs/" + runID + "/tail?offset=" + strconv.FormatInt(offset, 10)
	if stream != "" {
		url += "&stream=" + stream
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("tail = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var res tailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode tail: %v", err)
	}
	return res
}

// While a run is in progress the tail must report done=false so the UI keeps polling;
// once it finishes, done=true stops the polling.
func TestHandleRunTailFollowsRunToCompletion(t *testing.T) {
	srv, _ := resticTestServer(t, false)
	inst := &Instance{ID: "files-web01", Script: "files-backup", RunnerID: "web-01"}
	run, err := srv.store.CreateRun(inst, "", 3600)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// Pending: no output, not done.
	res := tailFor(t, srv, run.ID, 0, "")
	if res.Data != "" || res.Done || res.Status != StatusPending {
		t.Errorf("pending tail = %+v, want empty/not done/pending", res)
	}

	// Running with output.
	if err := srv.store.MarkRunStarted(run.ID, time.Now()); err != nil {
		t.Fatalf("MarkRunStarted: %v", err)
	}
	if _, err := srv.store.AppendOutput(run.ID, "stdout", []byte("line one\n")); err != nil {
		t.Fatalf("AppendOutput: %v", err)
	}
	res = tailFor(t, srv, run.ID, 0, "")
	if res.Data != "line one\n" || res.Done {
		t.Errorf("running tail = %+v, want the line and done=false", res)
	}
	if res.Status != StatusRunning {
		t.Errorf("status = %s, want running", res.Status)
	}

	// The next poll from the returned offset sees only what was added since.
	if _, err := srv.store.AppendOutput(run.ID, "stdout", []byte("line two\n")); err != nil {
		t.Fatalf("AppendOutput: %v", err)
	}
	res2 := tailFor(t, srv, run.ID, res.Offset, "")
	if res2.Data != "line two\n" {
		t.Errorf("incremental tail = %q, want only the new line", res2.Data)
	}

	// Finished: done=true tells the UI to stop.
	if err := srv.store.FinishRun(run.ID, time.Now(), 0, ""); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	res3 := tailFor(t, srv, run.ID, res2.Offset, "")
	if !res3.Done || res3.Status != StatusSuccess {
		t.Errorf("finished tail = %+v, want done=true/success", res3)
	}
}

func TestHandleRunTailUnknownRun(t *testing.T) {
	srv, _ := resticTestServer(t, false)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/runs/run-999/tail", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("tail of unknown run = %d, want 404", rec.Code)
	}
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/runs/nonsense/tail", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("tail of malformed run id = %d, want 404", rec.Code)
	}
}

func TestIsTerminal(t *testing.T) {
	terminal := []RunStatus{StatusSuccess, StatusFailed, StatusError}
	live := []RunStatus{StatusPending, StatusRunning}
	for _, st := range terminal {
		if !isTerminal(st) {
			t.Errorf("isTerminal(%s) = false, want true", st)
		}
	}
	for _, st := range live {
		if isTerminal(st) {
			t.Errorf("isTerminal(%s) = true, want false", st)
		}
	}
}

// The UI is embedded in the binary and served by the web listener; these routes must
// actually serve it.
func TestWebUIIsServed(t *testing.T) {
	srv, _ := resticTestServer(t, false)
	h := srv.WebHandler()

	tests := []struct {
		path      string
		wantSub   string
		wantCType string
	}{
		{"/", "<title>Arcatum</title>", "text/html"},
		{"/app.js", "pollTail", "javascript"},
		{"/style.css", "--accent", "text/css"},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200", tc.path, rec.Code)
			}
			if !strings.Contains(rec.Body.String(), tc.wantSub) {
				t.Errorf("GET %s does not contain %q", tc.path, tc.wantSub)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, tc.wantCType) {
				t.Errorf("GET %s Content-Type = %q, want %q", tc.path, ct, tc.wantCType)
			}
		})
	}

	// The file server canonicalises /index.html to the root, which is fine as long as
	// the route exists rather than 404ing.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/index.html", nil))
	if rec.Code != http.StatusMovedPermanently {
		t.Errorf("GET /index.html = %d, want a 301 redirect to /", rec.Code)
	}

	// The text status page stays available for shell use on the API listener.
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "arcatum-server") {
		t.Errorf("GET /status = %d, body %q", rec.Code, rec.Body.String())
	}
	// On the web listener it is data like any other, so it needs a login — unlike the
	// assets above, which are only the page that asks for one.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("web GET /status without a login = %d, want 401", rec.Code)
	}
}

// On the mTLS listener everything an operator can reach still needs an admin
// certificate. The browser UI is not served there at all — it lives on the web listener,
// where a password login is possible (see TestWebAPIRequiresLogin).
func TestOperatorAPIRequiresAdminCertificate(t *testing.T) {
	srv, _ := resticTestServer(t, true)
	h := srv.Handler()
	for _, path := range []string{"/", "/status", "/api/v1/runs", "/api/v1/instances", "/api/v1/whoami"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without a certificate = %d, want 401", path, rec.Code)
		}
	}
}
