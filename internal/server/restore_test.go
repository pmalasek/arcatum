package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"arcatum/pkg/crypto"
)

func TestCleanSnapshotPath(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"/", "/"},
		{"/etc/nginx", "/etc/nginx"},
		{"etc/nginx", "/etc/nginx"},          // made absolute
		{"/etc/nginx/", "/etc/nginx"},        // trailing slash dropped
		{"/etc//nginx", "/etc/nginx"},        // collapsed
		{"/etc/./nginx", "/etc/nginx"},       // normalised
		{"/etc/../etc/nginx", "/etc/nginx"},  // resolved
		{"/../../etc/passwd", "/etc/passwd"}, // cannot climb above the root
		{"..", "/"},
	}
	for _, tc := range tests {
		if got := cleanSnapshotPath(tc.in); got != tc.want {
			t.Errorf("cleanSnapshotPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
		// Whatever comes in, nothing with ".." may reach restic's arguments.
		if strings.Contains(cleanSnapshotPath(tc.in), "..") {
			t.Errorf("cleanSnapshotPath(%q) left a .. segment", tc.in)
		}
	}
}

func TestSnapshotIDValidation(t *testing.T) {
	valid := []string{
		strings.Repeat("a", 8),
		strings.Repeat("ab", 32),
		"48702509",
	}
	invalid := []string{
		"", "latest-ish", "../../etc", strings.Repeat("z", 8),
		strings.Repeat("a", 7), strings.Repeat("a", 9), "48702509;rm -rf /",
	}
	for _, id := range valid {
		if !resticSnapshotIDPattern.MatchString(id) {
			t.Errorf("snapshot id %q should be accepted", id)
		}
	}
	for _, id := range invalid {
		if resticSnapshotIDPattern.MatchString(id) {
			t.Errorf("snapshot id %q should be rejected", id)
		}
	}
}

// restic lists a snapshot recursively; the browsable view is built by filtering to one
// level here.
func TestParseResticLS(t *testing.T) {
	// Realistic ndjson: a snapshot header line followed by nodes.
	out := strings.Join([]string{
		`{"message_type":"snapshot","id":"48702509","paths":["/data"]}`,
		`{"message_type":"node","name":"data","type":"dir","path":"/data"}`,
		`{"message_type":"node","name":"a.txt","type":"file","path":"/data/a.txt","size":12,"mtime":"2026-07-26T08:00:00Z"}`,
		`{"message_type":"node","name":"b.bin","type":"file","path":"/data/b.bin","size":2048,"mtime":"2026-07-26T08:00:00Z"}`,
		`{"message_type":"node","name":"sub","type":"dir","path":"/data/sub"}`,
		`{"message_type":"node","name":"deep.txt","type":"file","path":"/data/sub/deep.txt","size":3}`,
	}, "\n")

	entries, truncated := parseResticLS([]byte(out), "/data", 100)
	if truncated {
		t.Error("should not be truncated")
	}
	got := map[string]RestoreEntry{}
	for _, e := range entries {
		got[e.Name] = e
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries %v, want a.txt, b.bin and sub", len(entries), names(entries))
	}
	if got["a.txt"].Type != "file" || got["a.txt"].Size != 12 {
		t.Errorf("a.txt = %+v", got["a.txt"])
	}
	if got["sub"].Type != "dir" {
		t.Errorf("sub = %+v, want a dir", got["sub"])
	}
	// The nested file must not leak into this level.
	if _, ok := got["deep.txt"]; ok {
		t.Error("deep.txt is one level down and must not be listed here")
	}

	// Descending shows the nested file.
	entries, _ = parseResticLS([]byte(out), "/data/sub", 100)
	if len(entries) != 1 || entries[0].Name != "deep.txt" {
		t.Errorf("listing /data/sub = %v, want deep.txt", names(entries))
	}
}

// A directory whose contents restic lists before the directory node itself must still
// appear, or parts of the tree would be unreachable.
func TestParseResticLSInfersMissingDirectories(t *testing.T) {
	out := strings.Join([]string{
		`{"message_type":"snapshot","id":"48702509"}`,
		`{"message_type":"node","name":"deep.txt","type":"file","path":"/data/sub/deep.txt","size":3}`,
	}, "\n")
	entries, _ := parseResticLS([]byte(out), "/data", 100)
	if len(entries) != 1 || entries[0].Name != "sub" || entries[0].Type != "dir" {
		t.Errorf("entries = %v, want an inferred dir 'sub'", names(entries))
	}
}

// Older restic versions label the lines struct_type rather than message_type.
func TestParseResticLSAcceptsLegacyFieldName(t *testing.T) {
	out := `{"struct_type":"node","name":"a.txt","type":"file","path":"/data/a.txt","size":1}`
	entries, _ := parseResticLS([]byte(out), "/data", 100)
	if len(entries) != 1 || entries[0].Name != "a.txt" {
		t.Errorf("entries = %v, want a.txt", names(entries))
	}
}

func TestParseResticLSTruncates(t *testing.T) {
	var lines []string
	for i := 0; i < 10; i++ {
		lines = append(lines,
			`{"message_type":"node","name":"f`+string(rune('0'+i))+`","type":"file","path":"/data/f`+string(rune('0'+i))+`"}`)
	}
	entries, truncated := parseResticLS([]byte(strings.Join(lines, "\n")), "/data", 3)
	if len(entries) != 3 || !truncated {
		t.Errorf("got %d entries, truncated=%v; want 3 and true", len(entries), truncated)
	}
}

func TestParseResticLSIgnoresGarbage(t *testing.T) {
	out := strings.Join([]string{
		`not json at all`,
		`{"message_type":"node","name":"a.txt","type":"file","path":"/data/a.txt"}`,
		`{`,
	}, "\n")
	entries, _ := parseResticLS([]byte(out), "/data", 100)
	if len(entries) != 1 {
		t.Errorf("entries = %v, want just a.txt (garbage lines skipped)", names(entries))
	}
}

func names(entries []RestoreEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name
	}
	return out
}

// Restore exposes backup contents, so it must be admin-only.
func TestRestoreEndpointsRequireAdmin(t *testing.T) {
	srv, _ := enrollTestServer(t, true)
	snap := strings.Repeat("ab", 32)
	paths := []string{
		"/api/v1/instances/files-web01/snapshots",
		"/api/v1/instances/files-web01/snapshots/" + snap + "/ls",
		"/api/v1/instances/files-web01/snapshots/" + snap + "/download?path=/etc",
	}
	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			// A runner certificate must not be enough.
			r := httptest.NewRequest(http.MethodGet, p, nil)
			r.TLS = requestWithCert(certWithCNAndRole("web-01", crypto.RoleRunner)).TLS
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, r)
			if rec.Code != http.StatusForbidden {
				t.Errorf("as runner = %d, want 403", rec.Code)
			}
			// Nor no certificate at all.
			rec = httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("without a certificate = %d, want 401", rec.Code)
			}
		})
	}
}

func TestRestoreRejectsBadIdentifiers(t *testing.T) {
	srv, _ := resticTestServer(t, false)
	tests := []struct {
		name string
		path string
	}{
		{"bad snapshot id", "/api/v1/instances/files-web01/snapshots/nonsense/ls"},
		{"snapshot id with injection", "/api/v1/instances/files-web01/snapshots/ab%3Bwhoami/ls"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s = %d, want 400", tc.path, rec.Code)
			}
		})
	}
	// A download needs a path to dump.
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/instances/files-web01/snapshots/latest/download", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("download without a path = %d, want 400", rec.Code)
	}
}

// An instance with no repository, or no repository password, must produce a clear error
// rather than an obscure restic failure.
func TestResticEnvErrors(t *testing.T) {
	srv, _ := resticTestServer(t, false) // instance files-web01 has no secrets
	dir := t.TempDir()

	if _, _, err := srv.resticEnv("files-web01", dir); err == nil ||
		!strings.Contains(err.Error(), resticPasswordSecret) {
		t.Errorf("error = %v, want it to name the missing secret", err)
	}
	if _, _, err := srv.resticEnv("does-not-exist", dir); err == nil ||
		!strings.Contains(err.Error(), "unknown instance") {
		t.Errorf("error = %v, want an unknown-instance error", err)
	}
}
