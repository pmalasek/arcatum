package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"sync"
	"testing"

	"arcatum/pkg/proto"
)

// dispatchFor builds a signed-shape dispatch running the given shell script.
func dispatchFor(t *testing.T, script, capture string) proto.JobDispatch {
	t.Helper()
	content := []byte(script)
	sum := sha256.Sum256(content)
	return proto.JobDispatch{
		RunID:      "run-1",
		InstanceID: "db-01",
		Script:     "test",
		Type:       proto.TypeBash,
		Capture:    capture,
		Artifact: proto.Artifact{
			Filename: "test.sh",
			SHA256:   hex.EncodeToString(sum[:]),
			Content:  content,
		},
	}
}

// collector gathers what Execute emitted, guarded because the streams are read
// concurrently.
type collector struct {
	mu       sync.Mutex
	stdout   strings.Builder
	stderr   strings.Builder
	finished *proto.RunUpdate
}

func (c *collector) emit(u proto.RunUpdate) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch u.Kind {
	case proto.KindOutput:
		if u.Stream == "stderr" {
			c.stderr.Write(u.Data)
		} else {
			c.stdout.Write(u.Data)
		}
	case proto.KindFinished:
		f := u
		c.finished = &f
	}
}

// For a streaming job the dump must go to the uploader, not into the run log — that is
// the whole reason the payload has its own path.
func TestExecuteStreamsPayloadInsteadOfLoggingIt(t *testing.T) {
	d := dispatchFor(t, "echo 'INSERT INTO t VALUES (1);'; echo 'dumping' >&2", proto.CaptureStream)
	var c collector
	var uploaded strings.Builder
	upload := func(_ context.Context, runID string, r io.Reader) (int64, error) {
		if runID != d.RunID {
			t.Errorf("upload got run %q, want %q", runID, d.RunID)
		}
		n, err := io.Copy(&uploaded, r)
		return n, err
	}

	Execute(context.Background(), d, t.TempDir(), c.emit, upload)

	if got := uploaded.String(); got != "INSERT INTO t VALUES (1);\n" {
		t.Errorf("uploaded %q, want the dump", got)
	}
	if strings.Contains(c.stdout.String(), "INSERT INTO") {
		t.Errorf("the dump reached the log: %q", c.stdout.String())
	}
	// The log should still say something happened, and stderr is unaffected.
	if !strings.Contains(c.stdout.String(), "streamed 26 bytes") {
		t.Errorf("stdout log = %q, want a summary of what was streamed", c.stdout.String())
	}
	if !strings.Contains(c.stderr.String(), "dumping") {
		t.Errorf("stderr = %q, want it logged as usual", c.stderr.String())
	}
	if c.finished == nil || c.finished.ExitCode != 0 || c.finished.Error != "" {
		t.Errorf("finished = %+v, want a clean exit", c.finished)
	}
}

// Without a streaming declaration stdout is a log, as it always was.
func TestExecuteLogsStdoutForANonStreamingJob(t *testing.T) {
	d := dispatchFor(t, "echo hello", proto.CaptureLog)
	var c collector
	upload := func(context.Context, string, io.Reader) (int64, error) {
		t.Error("a log job must not upload a payload")
		return 0, nil
	}

	Execute(context.Background(), d, t.TempDir(), c.emit, upload)

	if c.stdout.String() != "hello\n" {
		t.Errorf("stdout = %q, want it logged", c.stdout.String())
	}
}

// A backup that never arrived is not a successful backup, however cleanly the script
// itself exited.
func TestExecuteFailsTheRunWhenTheUploadFails(t *testing.T) {
	d := dispatchFor(t, "echo data", proto.CaptureStream)
	var c collector
	upload := func(context.Context, string, io.Reader) (int64, error) {
		return 0, io.ErrUnexpectedEOF
	}

	Execute(context.Background(), d, t.TempDir(), c.emit, upload)

	if c.finished == nil {
		t.Fatal("no finished update")
	}
	if c.finished.ExitCode == 0 || c.finished.Error == "" {
		t.Errorf("finished = %+v, want a failure naming the upload", c.finished)
	}
	if !strings.Contains(c.finished.Error, "upload") {
		t.Errorf("error = %q, want it to say the upload failed", c.finished.Error)
	}
}

// A script that fails is reported with its exit code, and the emitted log is unaffected.
func TestExecuteReportsScriptFailure(t *testing.T) {
	d := dispatchFor(t, "echo broken >&2; exit 3", proto.CaptureLog)
	var c collector

	Execute(context.Background(), d, t.TempDir(), c.emit, nil)

	if c.finished == nil || c.finished.ExitCode != 3 {
		t.Fatalf("finished = %+v, want exit code 3", c.finished)
	}
	if c.finished.Error != "" {
		t.Errorf("error = %q, want empty — a non-zero exit is not a runner error", c.finished.Error)
	}
	if !strings.Contains(c.stderr.String(), "broken") {
		t.Errorf("stderr = %q, want the script's message", c.stderr.String())
	}
}
