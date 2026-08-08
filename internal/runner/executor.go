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
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"arcatum/pkg/proto"
)

// DataUploader ships a run's backup payload to the server. It is separate from emit
// because the payload is not output to be logged: it is the backup itself, and routing
// it through the update stream would base64-encode a database dump into the run log.
// Nil means the job's stdout is treated as a log whatever the dispatch says, which is
// what a caller with nowhere to put a payload (the tests) needs.
type DataUploader func(ctx context.Context, runID string, r io.Reader) (int64, error)

// Execute runs one dispatched job. It emits RunUpdate values (started, output
// chunks, finished) through emit, and for a job declared capture = "stream" hands the
// process's stdout to upload instead of logging it. All temp files live under a per-run
// dir that is removed on return.
func Execute(ctx context.Context, d proto.JobDispatch, baseDir string, emit func(proto.RunUpdate), upload DataUploader) {
	emit(proto.RunUpdate{RunID: d.RunID, Kind: proto.KindStarted})

	finish := func(exit int, execErr error) {
		u := proto.RunUpdate{RunID: d.RunID, Kind: proto.KindFinished, ExitCode: exit}
		if execErr != nil {
			u.Error = execErr.Error()
		}
		emit(u)
	}

	// Cancellable from inside as well as from the caller: when the payload turns out to
	// have nowhere to go, letting the script run to the end would keep a dump hammering
	// the database to produce bytes that are already being thrown away.
	runCtx, abort := context.WithCancel(ctx)
	defer abort()

	workDir, err := os.MkdirTemp(baseDir, "run-"+d.RunID+"-")
	if err != nil {
		finish(-1, fmt.Errorf("mktemp: %w", err))
		return
	}
	defer os.RemoveAll(workDir)

	cmd, err := prepare(runCtx, d, workDir)
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
	var uploaded int64
	var uploadErr error
	var uploadFailed bool // the upload broke on its own, rather than being interrupted
	wg.Add(2)
	if proto.StreamsPayload(d.Capture) && upload != nil {
		go func() {
			defer wg.Done()
			uploaded, uploadErr = upload(runCtx, d.RunID, stdout)
			if uploadErr != nil {
				// A cancelled run fails its upload too; that is not the upload's fault and
				// must not be reported as one.
				if runCtx.Err() == nil {
					uploadFailed = true
					abort()
				}
				// Nobody is reading stdout any more. Drain it, or the process blocks on a
				// full pipe and cmd.Wait below never returns.
				io.Copy(io.Discard, stdout)
			}
		}()
	} else {
		go streamPipe(&wg, stdout, "stdout", d.RunID, emit)
	}
	go streamPipe(&wg, stderr, "stderr", d.RunID, emit)
	// Waiting for the upload before finishing is what keeps the two in step: the server
	// promotes the payload when the run is marked successful, so that verdict must not be
	// sent until the upload has either completed or failed.
	wg.Wait()

	err = cmd.Wait()
	if runCtx.Err() != nil {
		killProcessGroup(cmd)
	}
	code, execErr := exitCode(err), nonExitError(err)
	switch {
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		code, execErr = -1, fmt.Errorf("timed out after %d seconds", d.TimeoutSec)
	case uploadFailed:
		// The script may well have exited cleanly — it wrote its dump. Without this the
		// run would be recorded as a successful backup that never arrived.
		code, execErr = -1, fmt.Errorf("uploading backup data: %w", uploadErr)
	case runCtx.Err() != nil:
		// Stopped from outside: an operator asked for it, or the runner is shutting down.
		// The killed process's exit code says enough, and the server decides between
		// "cancelled" and "failed" from the request it recorded.
	case uploaded > 0:
		// The run log of a streaming job would otherwise be empty; say what was sent.
		emit(proto.RunUpdate{RunID: d.RunID, Kind: proto.KindOutput, Stream: "stdout",
			Data: []byte(fmt.Sprintf("streamed %d bytes of backup data to the server\n", uploaded))})
	}
	finish(code, execErr)
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
	setupProcessGroup(cmd)
	return cmd, nil
}

// killGrace is how long a job gets to shut down after being signalled before it is taken
// apart. Enough for mysqldump to close a connection, not enough to hold up an operator
// who has asked for the run to stop.
const killGrace = 10 * time.Second

// setupProcessGroup makes a cancelled run actually stop.
//
// The artifact is a shell script, but what produces the backup is a child of it —
// `mysqldump`, or a whole pipeline. Killing the interpreter alone leaves those running,
// still holding the write end of the pipes this run reads from, so the read never ends,
// cmd.Wait never returns, and the run hangs having "stopped". Worse, a dump would keep
// running against the database nobody is watching any more.
//
// So the job gets its own process group and the signal goes to the group. WaitDelay is
// the backstop for anything that ignores it: the pipes get closed and the run ends
// regardless.
func setupProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// A negative pid signals the whole process group.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = killGrace
}

// killProcessGroup removes whatever survived the polite signal. Called only for a run
// that was cancelled or timed out: after a normal exit there is nothing to clean up, and
// a script that deliberately left something running is not ours to kill.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	// ESRCH simply means the group is already gone, which is the expected case.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
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
