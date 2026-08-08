package server

import (
	"testing"

	"arcatum/pkg/jobspec"
	"arcatum/pkg/proto"
)

// What stdout is comes from the script, not from the instance. Instances predate the
// manifest declaration and many carry capture = "stream" for scripts that only ever
// printed text; honouring those would divert a hello-world's output into a payload file.
func TestEffectiveCapture(t *testing.T) {
	streaming := &jobspec.Manifest{Name: "mysql-backup", Type: proto.TypeBash, Capture: proto.CaptureStream}
	logging := &jobspec.Manifest{Name: "hello", Type: proto.TypeBash}
	restic := &jobspec.Manifest{Name: "files-backup", Type: proto.TypeRestic}

	tests := []struct {
		name     string
		manifest *jobspec.Manifest
		instance string // the instance's capture field
		want     string
	}{
		{"streaming script", streaming, "", proto.CaptureStream},
		{"streaming script, instance agrees", streaming, proto.CaptureStream, proto.CaptureStream},
		{"streaming script, instance opts out", streaming, proto.CaptureLocal, proto.CaptureLog},
		{"logging script", logging, "", proto.CaptureLog},
		{"logging script with a stale instance value", logging, proto.CaptureStream, proto.CaptureLog},
		{"restic job", restic, proto.CaptureStream, proto.CaptureLog},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := effectiveCapture(tc.manifest, &Instance{Capture: tc.instance})
			if got != tc.want {
				t.Errorf("effectiveCapture = %q, want %q", got, tc.want)
			}
		})
	}
}
