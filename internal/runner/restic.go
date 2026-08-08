package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"arcatum/pkg/proto"
)

// File backups are delegated to restic rather than reimplemented: deduplication,
// incremental snapshots, compression, encryption and integrity checking are exactly
// the parts that take years to get right.
//
// The repository lives on the Arcatum server, reached through its restic REST
// backend, so pack files stream off the backed-up host instead of piling up on it.
// Only restic's local cache stays behind.
//
// Parameters (from the instance):
//
//	paths        required, comma-separated list of paths to back up
//	excludes     optional, comma-separated exclude patterns
//	tags         optional, comma-separated extra tags
//	keep_daily   optional retention; when any keep_* is set, forget --prune runs
//	keep_weekly  after a successful backup
//	keep_monthly
//	keep_last
//
// Secret: restic_password — the repository password.

const resticPasswordSecret = "restic_password"

// resticParams is the parsed instance configuration for a restic job.
type resticParams struct {
	paths    []string
	excludes []string
	tags     []string
	keep     map[string]string // restic flag -> value, e.g. "--keep-daily" -> "7"
}

var keepFlags = map[string]string{
	"keep_last":    "--keep-last",
	"keep_daily":   "--keep-daily",
	"keep_weekly":  "--keep-weekly",
	"keep_monthly": "--keep-monthly",
	"keep_yearly":  "--keep-yearly",
}

// parseResticParams validates the instance parameters for a restic job.
func parseResticParams(d proto.JobDispatch) (*resticParams, error) {
	p := &resticParams{keep: map[string]string{}}
	p.paths = splitList(d.Params["paths"])
	if len(p.paths) == 0 {
		return nil, fmt.Errorf("restic job needs a %q parameter listing what to back up", "paths")
	}
	p.excludes = splitList(d.Params["excludes"])
	p.tags = splitList(d.Params["tags"])

	for param, flag := range keepFlags {
		raw, ok := d.Params[param]
		if !ok || strings.TrimSpace(raw) == "" {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || n < 0 {
			return nil, fmt.Errorf("parameter %s must be a non-negative number, got %q", param, raw)
		}
		p.keep[flag] = strconv.Itoa(n)
	}
	if _, ok := d.Secrets[resticPasswordSecret]; !ok {
		return nil, fmt.Errorf("restic job needs a %q secret (the repository password)", resticPasswordSecret)
	}
	return p, nil
}

// splitList parses a comma-separated parameter value.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// resticRepoURL builds the REST repository URL for an instance. restic requires the
// trailing slash for a REST repository.
func resticRepoURL(serverBase, instanceID string) string {
	return "rest:" + strings.TrimSuffix(serverBase, "/") + "/restic/" + instanceID + "/"
}

// backupArgs builds the "restic backup" arguments. Split out so the command can be
// unit-tested without running restic.
func (p *resticParams) backupArgs(instanceID string) []string {
	args := []string{"backup", "--tag", "arcatum", "--tag", "instance:" + instanceID}
	for _, t := range p.tags {
		args = append(args, "--tag", t)
	}
	for _, e := range p.excludes {
		args = append(args, "--exclude", e)
	}
	return append(args, p.paths...)
}

// forgetArgs builds the retention command, or nil when no retention is configured.
// It is scoped by tag so one instance's policy never deletes another's snapshots.
func (p *resticParams) forgetArgs(instanceID string) []string {
	if len(p.keep) == 0 {
		return nil
	}
	args := []string{"forget", "--prune", "--tag", "instance:" + instanceID}
	// Sorted for a deterministic command line.
	for _, param := range []string{"keep_last", "keep_daily", "keep_weekly", "keep_monthly", "keep_yearly"} {
		flag := keepFlags[param]
		if v, ok := p.keep[flag]; ok {
			args = append(args, flag, v)
		}
	}
	return args
}

// executeRestic runs a restic file backup for one dispatch.
func (a *Agent) executeRestic(ctx context.Context, d proto.JobDispatch, emit func(proto.RunUpdate)) {
	emit(proto.RunUpdate{RunID: d.RunID, Kind: proto.KindStarted})

	finish := func(exit int, err error) {
		u := proto.RunUpdate{RunID: d.RunID, Kind: proto.KindFinished, ExitCode: exit}
		if err != nil {
			u.Error = err.Error()
		}
		emit(u)
	}
	logLine := func(format string, args ...any) {
		emit(proto.RunUpdate{RunID: d.RunID, Kind: proto.KindOutput, Stream: "stdout",
			Data: []byte(fmt.Sprintf(format+"\n", args...))})
	}

	resticBin, err := exec.LookPath("restic")
	if err != nil {
		finish(-1, fmt.Errorf("restic not found on this host: %w", err))
		return
	}
	params, err := parseResticParams(d)
	if err != nil {
		finish(-1, err)
		return
	}

	workDir, err := os.MkdirTemp(a.workBase, "restic-"+d.RunID+"-")
	if err != nil {
		finish(-1, fmt.Errorf("mktemp: %w", err))
		return
	}
	defer os.RemoveAll(workDir)

	// The repository password lives in a file for the duration of the run only.
	pwFile := filepath.Join(workDir, "repo-password")
	if err := os.WriteFile(pwFile, []byte(d.Secrets[resticPasswordSecret]), 0o600); err != nil {
		finish(-1, fmt.Errorf("write password file: %w", err))
		return
	}

	repo := resticRepoURL(a.client.BaseURL(), d.InstanceID)
	base := []string{"--repo", repo, "--password-file", pwFile}

	// restic talks to the server itself, so it needs the same mTLS material.
	tlsArgs, cleanup, err := a.resticTLSArgs(workDir)
	if err != nil {
		finish(-1, err)
		return
	}
	defer cleanup()
	base = append(base, tlsArgs...)

	run := func(args ...string) (int, error) {
		cmd := exec.CommandContext(ctx, resticBin, append(base, args...)...)
		cmd.Dir = workDir
		cmd.Env = append(os.Environ(), "RESTIC_CACHE_DIR="+filepath.Join(a.workBase, "restic-cache"))
		return streamCommand(cmd, d.RunID, emit)
	}

	// Initialise the repository on first use. "cat config" is the cheap probe restic
	// itself uses to tell an existing repository from a missing one.
	probe := exec.CommandContext(ctx, resticBin, append(base, "cat", "config")...)
	probe.Env = append(os.Environ(), "RESTIC_CACHE_DIR="+filepath.Join(a.workBase, "restic-cache"))
	if err := probe.Run(); err != nil {
		logLine("repository not initialised yet, creating it")
		if code, err := run("init"); err != nil || code != 0 {
			finish(code, fmt.Errorf("restic init failed: %v", err))
			return
		}
	}

	logLine("backing up: %s", strings.Join(params.paths, ", "))
	code, err := run(params.backupArgs(d.InstanceID)...)
	if err != nil || code != 0 {
		finish(code, err)
		return
	}

	// Retention runs only after a successful backup, so a failed run can never be the
	// reason old snapshots disappear.
	if args := params.forgetArgs(d.InstanceID); args != nil {
		logLine("applying retention policy")
		if code, err := run(args...); err != nil || code != 0 {
			finish(code, fmt.Errorf("retention (forget --prune) failed: %v", err))
			return
		}
	}
	finish(0, nil)
}

// resticTLSArgs writes the client certificate in the single-file form restic expects
// and returns the flags for it.
func (a *Agent) resticTLSArgs(workDir string) (args []string, cleanup func(), err error) {
	cleanup = func() {}
	if a.tls.CACert == "" {
		return nil, cleanup, nil // development mode: plain HTTP
	}
	args = append(args, "--cacert", a.tls.CACert)
	if a.tls.Cert == "" || a.tls.Key == "" {
		return args, cleanup, nil
	}
	certPEM, err := os.ReadFile(a.tls.Cert)
	if err != nil {
		return nil, cleanup, fmt.Errorf("read client certificate: %w", err)
	}
	keyPEM, err := os.ReadFile(a.tls.Key)
	if err != nil {
		return nil, cleanup, fmt.Errorf("read client key: %w", err)
	}
	// restic wants certificate and key concatenated in one file.
	combined := filepath.Join(workDir, "client.pem")
	if err := os.WriteFile(combined, append(certPEM, keyPEM...), 0o600); err != nil {
		return nil, cleanup, fmt.Errorf("write client certificate: %w", err)
	}
	return append(args, "--tls-client-cert", combined), func() { os.Remove(combined) }, nil
}

// streamCommand runs a command, forwarding both streams to the server, and returns its
// exit code.
func streamCommand(cmd *exec.Cmd, runID string, emit func(proto.RunUpdate)) (int, error) {
	// Same reason as for scripts: a cancelled run has to take the whole job with it, not
	// just the process we started (see setupProcessGroup).
	setupProcessGroup(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return -1, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return -1, err
	}
	if err := cmd.Start(); err != nil {
		return -1, fmt.Errorf("start %s: %w", cmd.Path, err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go streamPipe(&wg, stdout, "stdout", runID, emit)
	go streamPipe(&wg, stderr, "stderr", runID, emit)
	wg.Wait()

	waitErr := cmd.Wait()
	return exitCode(waitErr), nonExitError(waitErr)
}
