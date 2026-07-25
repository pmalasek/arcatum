// Package proto defines the wire messages exchanged between arcatum-server and
// arcatum-runner. The runner initiates all communication (pull model): it checks
// in over outbound HTTPS/mTLS, receives due work, and streams results back.
package proto

// Version is the protocol version. Bump on breaking changes.
const Version = "v1"

// ScriptType identifies how the runner executes a script.
type ScriptType string

const (
	TypeBash   ScriptType = "bash"
	TypePython ScriptType = "python"
	TypeBinary ScriptType = "binary"
	TypeRestic ScriptType = "restic"
)

// CheckinRequest is sent by a runner to ask the server for due work.
type CheckinRequest struct {
	RunnerID string `json:"runner_id"`
	Hostname string `json:"hostname"`
	OS       string `json:"os"`   // linux, ...
	Arch     string `json:"arch"` // amd64, arm64 — used to pick binary artifacts
}

// CheckinResponse returns the jobs the runner should execute now.
type CheckinResponse struct {
	Due []JobDispatch `json:"due"`
}

// JobDispatch is a single signed unit of work handed to a runner. It binds a
// script definition to one instance's parameters for one execution.
type JobDispatch struct {
	RunID      string            `json:"run_id"`      // server-assigned id for this execution
	InstanceID string            `json:"instance_id"` // which instance (script + target + params)
	Script     string            `json:"script"`      // script (definition) name
	Type       ScriptType        `json:"type"`
	Artifact   Artifact          `json:"artifact"` // the code/binary to run
	Params     map[string]string `json:"params"`   // non-secret params -> env vars
	Secrets    map[string]string `json:"secrets"`  // decrypted only for this runner over mTLS -> temp file
	TimeoutSec int               `json:"timeout_sec"`
	Capture    string            `json:"capture"`   // "stream" | "local"
	Signature  []byte            `json:"signature"` // server signature over the dispatch (runner verifies)
}

// Artifact carries the executable content (or a reference) plus its hash.
type Artifact struct {
	Filename string `json:"filename"`
	SHA256   string `json:"sha256"`
	Content  []byte `json:"content,omitempty"` // inline for small scripts; large binaries fetched separately
}

// UpdateKind classifies a RunUpdate.
type UpdateKind string

const (
	KindStarted  UpdateKind = "started"
	KindOutput   UpdateKind = "output"
	KindFinished UpdateKind = "finished"
)

// RunUpdate streams execution progress and output back to the server, where it
// is stored. Keeping output on the server (not the backed-up host) is a design goal.
type RunUpdate struct {
	RunID    string     `json:"run_id"`
	Kind     UpdateKind `json:"kind"`
	Stream   string     `json:"stream,omitempty"` // "stdout" | "stderr" for KindOutput
	Data     []byte     `json:"data,omitempty"`
	ExitCode int        `json:"exit_code,omitempty"`
	Error    string     `json:"error,omitempty"`
}
