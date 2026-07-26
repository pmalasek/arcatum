package runner

import (
	"strings"
	"testing"

	"arcatum/pkg/proto"
)

func resticDispatch(params map[string]string, secrets map[string]string) proto.JobDispatch {
	return proto.JobDispatch{
		RunID:      "run-1",
		InstanceID: "files-web01",
		Script:     "files-backup",
		Type:       proto.TypeRestic,
		Params:     params,
		Secrets:    secrets,
	}
}

func TestResticRepoURL(t *testing.T) {
	tests := []struct {
		base string
		want string
	}{
		{"https://172.24.0.60:8443", "rest:https://172.24.0.60:8443/restic/files-web01/"},
		// A trailing slash on the base must not produce a double slash.
		{"https://172.24.0.60:8443/", "rest:https://172.24.0.60:8443/restic/files-web01/"},
		{"http://127.0.0.1:18443", "rest:http://127.0.0.1:18443/restic/files-web01/"},
	}
	for _, tc := range tests {
		if got := resticRepoURL(tc.base, "files-web01"); got != tc.want {
			t.Errorf("resticRepoURL(%q) = %q, want %q", tc.base, got, tc.want)
		}
	}
}

func TestParseResticParams(t *testing.T) {
	d := resticDispatch(
		map[string]string{
			"paths":      "/etc, /var/www",
			"excludes":   "*.tmp,/var/www/cache",
			"tags":       "nightly",
			"keep_daily": "7",
		},
		map[string]string{resticPasswordSecret: "pw"},
	)
	p, err := parseResticParams(d)
	if err != nil {
		t.Fatalf("parseResticParams: %v", err)
	}
	if len(p.paths) != 2 || p.paths[0] != "/etc" || p.paths[1] != "/var/www" {
		t.Errorf("paths = %v, want [/etc /var/www] (whitespace trimmed)", p.paths)
	}
	if len(p.excludes) != 2 || len(p.tags) != 1 {
		t.Errorf("excludes/tags = %v/%v", p.excludes, p.tags)
	}
	if p.keep["--keep-daily"] != "7" {
		t.Errorf("keep = %v, want --keep-daily 7", p.keep)
	}
}

// Misconfiguration must be caught before restic runs, with a message that says what to fix.
func TestParseResticParamsRejectsBadInput(t *testing.T) {
	tests := []struct {
		name    string
		params  map[string]string
		secrets map[string]string
		wantMsg string
	}{
		{
			name:    "no paths",
			params:  map[string]string{},
			secrets: map[string]string{resticPasswordSecret: "pw"},
			wantMsg: "paths",
		},
		{
			name:    "blank paths",
			params:  map[string]string{"paths": " , "},
			secrets: map[string]string{resticPasswordSecret: "pw"},
			wantMsg: "paths",
		},
		{
			name:    "missing password",
			params:  map[string]string{"paths": "/etc"},
			secrets: map[string]string{},
			wantMsg: resticPasswordSecret,
		},
		{
			name:    "non-numeric retention",
			params:  map[string]string{"paths": "/etc", "keep_daily": "many"},
			secrets: map[string]string{resticPasswordSecret: "pw"},
			wantMsg: "keep_daily",
		},
		{
			name:    "negative retention",
			params:  map[string]string{"paths": "/etc", "keep_daily": "-1"},
			secrets: map[string]string{resticPasswordSecret: "pw"},
			wantMsg: "keep_daily",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseResticParams(resticDispatch(tc.params, tc.secrets))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error %q does not mention %q", err, tc.wantMsg)
			}
		})
	}
}

// An empty retention value means "not set", not "keep zero" — which would delete
// every snapshot.
func TestParseResticParamsIgnoresEmptyRetention(t *testing.T) {
	p, err := parseResticParams(resticDispatch(
		map[string]string{"paths": "/etc", "keep_daily": "", "keep_weekly": "  "},
		map[string]string{resticPasswordSecret: "pw"},
	))
	if err != nil {
		t.Fatalf("parseResticParams: %v", err)
	}
	if len(p.keep) != 0 {
		t.Errorf("keep = %v, want empty", p.keep)
	}
	if p.forgetArgs("files-web01") != nil {
		t.Error("no retention configured, so forget must not run")
	}
}

func TestBackupArgs(t *testing.T) {
	p := &resticParams{
		paths:    []string{"/etc", "/var/www"},
		excludes: []string{"*.tmp"},
		tags:     []string{"nightly"},
	}
	args := p.backupArgs("files-web01")
	joined := strings.Join(args, " ")

	if args[0] != "backup" {
		t.Errorf("first argument = %q, want backup", args[0])
	}
	// Snapshots are tagged so retention and listings can be scoped per instance.
	if !strings.Contains(joined, "--tag arcatum") || !strings.Contains(joined, "--tag instance:files-web01") {
		t.Errorf("args %q missing the arcatum/instance tags", joined)
	}
	if !strings.Contains(joined, "--tag nightly") {
		t.Errorf("args %q missing the user tag", joined)
	}
	if !strings.Contains(joined, "--exclude *.tmp") {
		t.Errorf("args %q missing the exclude", joined)
	}
	// Paths must come last, after all flags.
	if args[len(args)-2] != "/etc" || args[len(args)-1] != "/var/www" {
		t.Errorf("args %q must end with the paths", joined)
	}
}

func TestForgetArgs(t *testing.T) {
	p := &resticParams{keep: map[string]string{
		"--keep-daily":   "7",
		"--keep-monthly": "6",
	}}
	args := p.forgetArgs("files-web01")
	joined := strings.Join(args, " ")

	if args[0] != "forget" || !strings.Contains(joined, "--prune") {
		t.Errorf("args %q, want a forget --prune command", joined)
	}
	// Scoping by tag keeps one instance's policy from deleting another's snapshots.
	if !strings.Contains(joined, "--tag instance:files-web01") {
		t.Errorf("args %q not scoped to the instance", joined)
	}
	if !strings.Contains(joined, "--keep-daily 7") || !strings.Contains(joined, "--keep-monthly 6") {
		t.Errorf("args %q missing the retention flags", joined)
	}
	// The order is fixed so the command is reproducible.
	if strings.Index(joined, "--keep-daily") > strings.Index(joined, "--keep-monthly") {
		t.Errorf("retention flags should be in a deterministic order: %q", joined)
	}
}

// restic wants the client certificate and key concatenated in one file.
func TestResticTLSArgs(t *testing.T) {
	dir := t.TempDir()
	certPath := dir + "/client.crt"
	keyPath := dir + "/client.key"
	writeTestFile(t, certPath, "CERT-PEM\n")
	writeTestFile(t, keyPath, "KEY-PEM\n")

	a := &Agent{tls: TLSFiles{CACert: dir + "/ca.pem", Cert: certPath, Key: keyPath}}
	args, cleanup, err := a.resticTLSArgs(dir)
	if err != nil {
		t.Fatalf("resticTLSArgs: %v", err)
	}
	defer cleanup()

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--cacert "+dir+"/ca.pem") {
		t.Errorf("args %q missing --cacert", joined)
	}
	combined := findFlagValue(args, "--tls-client-cert")
	if combined == "" {
		t.Fatalf("args %q missing --tls-client-cert", joined)
	}
	data := readTestFile(t, combined)
	if !strings.Contains(data, "CERT-PEM") || !strings.Contains(data, "KEY-PEM") {
		t.Errorf("combined PEM %q must contain both the certificate and the key", data)
	}
}

// Without mTLS there is nothing to pass to restic.
func TestResticTLSArgsEmptyInDevMode(t *testing.T) {
	a := &Agent{}
	args, cleanup, err := a.resticTLSArgs(t.TempDir())
	if err != nil {
		t.Fatalf("resticTLSArgs: %v", err)
	}
	defer cleanup()
	if len(args) != 0 {
		t.Errorf("args = %v, want none", args)
	}
}

func findFlagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
