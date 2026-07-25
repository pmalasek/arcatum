// Package runner implements arcatum-runner's execution side: it materializes a
// dispatched job, runs it, and streams output back via a callback. Non-secret
// params arrive as ARCATUM_<NAME> env vars; secrets are written to a short-lived
// file (path in ARCATUM_SECRETS_FILE) that is removed after the run — never env,
// which is visible in /proc/<pid>/environ.
package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"arcatum/pkg/proto"
)

// Execute runs one dispatched job. It emits RunUpdate values (started, output
// chunks, finished) through emit. All temp files live under a per-run dir that is
// removed on return.
func Execute(ctx context.Context, d proto.JobDispatch, baseDir string, emit func(proto.RunUpdate)) {
	emit(proto.RunUpdate{RunID: d.RunID, Kind: proto.KindStarted})

	finish := func(exit int, execErr error) {
		u := proto.RunUpdate{RunID: d.RunID, Kind: proto.KindFinished, ExitCode: exit}
		if execErr != nil {
			u.Error = execErr.Error()
		}
		emit(u)
	}

	workDir, err := os.MkdirTemp(baseDir, "run-"+d.RunID+"-")
	if err != nil {
		finish(-1, fmt.Errorf("mktemp: %w", err))
		return
	}
	defer os.RemoveAll(workDir)

	cmd, err := prepare(ctx, d, workDir)
	if err != nil {
		finish(-1, err)
		return
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		finish(-1, err)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		finish(-1, err)
		return
	}
	if err := cmd.Start(); err != nil {
		finish(-1, fmt.Errorf("start: %w", err))
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go streamPipe(&wg, stdout, "stdout", d.RunID, emit)
	go streamPipe(&wg, stderr, "stderr", d.RunID, emit)
	wg.Wait()

	err = cmd.Wait()
	finish(exitCode(err), nonExitError(err))
}

// prepare validates the artifact hash, writes the executable and secrets file, and
// builds the exec.Cmd with the right interpreter and environment.
func prepare(ctx context.Context, d proto.JobDispatch, workDir string) (*exec.Cmd, error) {
	// The signature covers the artifact's hash, not its bytes, so this check is what
	// ties the verified dispatch to the code actually about to run. It is mandatory.
	if d.Artifact.SHA256 == "" {
		return nil, fmt.Errorf("artifact has no sha256")
	}
	if got := sha256Hex(d.Artifact.Content); got != d.Artifact.SHA256 {
		return nil, fmt.Errorf("artifact hash mismatch: got %s want %s", got, d.Artifact.SHA256)
	}
	exe := filepath.Join(workDir, filepath.Base(d.Artifact.Filename))
	if err := os.WriteFile(exe, d.Artifact.Content, 0o700); err != nil {
		return nil, fmt.Errorf("write artifact: %w", err)
	}

	env := os.Environ()
	for k, v := range d.Params {
		env = append(env, envVar(k, v))
	}
	if len(d.Secrets) > 0 {
		secretsFile := filepath.Join(workDir, "secrets.env")
		var b strings.Builder
		for k, v := range d.Secrets {
			// shell-sourceable: export ARCATUM_KEY='value'
			fmt.Fprintf(&b, "export %s=%s\n", "ARCATUM_"+normalize(k), shellQuote(v))
		}
		if err := os.WriteFile(secretsFile, []byte(b.String()), 0o600); err != nil {
			return nil, fmt.Errorf("write secrets: %w", err)
		}
		env = append(env, "ARCATUM_SECRETS_FILE="+secretsFile)
	}

	var cmd *exec.Cmd
	switch d.Type {
	case proto.TypeBash:
		cmd = exec.CommandContext(ctx, "bash", exe)
	case proto.TypePython:
		cmd = exec.CommandContext(ctx, "python3", exe)
	case proto.TypeBinary:
		cmd = exec.CommandContext(ctx, exe)
	case proto.TypeRestic:
		// Restic orchestration lands later; for now treat the script as a wrapper.
		cmd = exec.CommandContext(ctx, "bash", exe)
	default:
		return nil, fmt.Errorf("unsupported script type %q", d.Type)
	}
	cmd.Env = env
	cmd.Dir = workDir
	return cmd, nil
}

func streamPipe(wg *sync.WaitGroup, r io.Reader, stream, runID string, emit func(proto.RunUpdate)) {
	defer wg.Done()
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			emit(proto.RunUpdate{RunID: runID, Kind: proto.KindOutput, Stream: stream, Data: chunk})
		}
		if err != nil {
			return
		}
	}
}

func envVar(key, val string) string { return "ARCATUM_" + normalize(key) + "=" + val }

// normalize upper-cases a param name and replaces non-alphanumerics with '_'.
func normalize(k string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(k) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// shellQuote single-quotes a value for a sourceable env file.
func shellQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// exitCode extracts the process exit code from cmd.Wait's error.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if ok := asExitError(err, &ee); ok {
		return ee.ExitCode()
	}
	return -1
}

// nonExitError returns err unless it is a plain non-zero exit (already conveyed by
// the exit code), in which case it returns nil.
func nonExitError(err error) error {
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	if asExitError(err, &ee) {
		return nil
	}
	return err
}

func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}
