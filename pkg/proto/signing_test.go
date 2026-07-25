package proto

import (
	"bytes"
	"testing"
)

func sampleDispatch() JobDispatch {
	return JobDispatch{
		RunID:      "run-1",
		InstanceID: "mysql-web01",
		Script:     "mysql-backup",
		Type:       TypeBash,
		Artifact:   Artifact{Filename: "mysql_backup.sh", SHA256: "abc123", Content: []byte("#!/bin/bash\n")},
		Params:     map[string]string{"host": "127.0.0.1", "database": "shop"},
		Secrets:    map[string]string{"password": "hunter2"},
		TimeoutSec: 3600,
		Capture:    "stream",
	}
}

func TestSigningBytesIsDeterministic(t *testing.T) {
	d := sampleDispatch()
	if !bytes.Equal(d.SigningBytes(), d.SigningBytes()) {
		t.Error("SigningBytes must be stable across calls")
	}
}

// Map iteration order in Go is random, so the encoding must sort keys — otherwise
// verification would fail intermittently.
func TestSigningBytesIgnoresMapOrder(t *testing.T) {
	a := sampleDispatch()
	b := sampleDispatch()
	b.Params = map[string]string{"database": "shop", "host": "127.0.0.1"} // inserted in another order
	if !bytes.Equal(a.SigningBytes(), b.SigningBytes()) {
		t.Error("SigningBytes must not depend on map insertion order")
	}
}

// The signature is attached to the dispatch, so it cannot itself be signed.
func TestSigningBytesExcludesSignature(t *testing.T) {
	a := sampleDispatch()
	b := sampleDispatch()
	b.Signature = []byte("some signature")
	if !bytes.Equal(a.SigningBytes(), b.SigningBytes()) {
		t.Error("the Signature field must be excluded from SigningBytes")
	}
}

// Artifact content is pinned through its hash, not signed directly.
func TestSigningBytesCoversHashNotContent(t *testing.T) {
	a := sampleDispatch()
	b := sampleDispatch()
	b.Artifact.Content = []byte("different content")
	if !bytes.Equal(a.SigningBytes(), b.SigningBytes()) {
		t.Error("content is verified via SHA256, so it must not enter SigningBytes")
	}

	c := sampleDispatch()
	c.Artifact.SHA256 = "deadbeef"
	if bytes.Equal(a.SigningBytes(), c.SigningBytes()) {
		t.Error("changing the artifact hash must change SigningBytes")
	}
}

func TestSigningBytesDetectsEveryFieldChange(t *testing.T) {
	base := sampleDispatch().SigningBytes()
	tests := []struct {
		name   string
		mutate func(*JobDispatch)
	}{
		{"run id", func(d *JobDispatch) { d.RunID = "run-2" }},
		{"instance id", func(d *JobDispatch) { d.InstanceID = "mysql-web02" }},
		{"script", func(d *JobDispatch) { d.Script = "evil" }},
		{"type", func(d *JobDispatch) { d.Type = TypeBinary }},
		{"artifact filename", func(d *JobDispatch) { d.Artifact.Filename = "other.sh" }},
		{"timeout", func(d *JobDispatch) { d.TimeoutSec = 60 }},
		{"capture", func(d *JobDispatch) { d.Capture = "local" }},
		{"param value", func(d *JobDispatch) { d.Params["host"] = "10.0.0.1" }},
		{"param added", func(d *JobDispatch) { d.Params["extra"] = "x" }},
		{"param removed", func(d *JobDispatch) { delete(d.Params, "host") }},
		{"secret value", func(d *JobDispatch) { d.Secrets["password"] = "other" }},
		{"secret added", func(d *JobDispatch) { d.Secrets["extra"] = "x" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := sampleDispatch()
			tc.mutate(&d)
			if bytes.Equal(base, d.SigningBytes()) {
				t.Errorf("changing %s did not change SigningBytes — the signature would not protect it", tc.name)
			}
		})
	}
}

// Length prefixes exist so no value can imitate a field boundary: moving text between
// two adjacent fields must produce different bytes.
func TestSigningBytesResistsFieldBoundaryShifting(t *testing.T) {
	a := sampleDispatch()
	a.RunID, a.InstanceID = "run", "1mysql-web01"
	b := sampleDispatch()
	b.RunID, b.InstanceID = "run1", "mysql-web01"
	if bytes.Equal(a.SigningBytes(), b.SigningBytes()) {
		t.Error("shifting characters across a field boundary must change SigningBytes")
	}
}

// Distinguishing an absent map from an empty one keeps the encoding unambiguous.
func TestSigningBytesNilAndEmptyMapsAgree(t *testing.T) {
	a := sampleDispatch()
	a.Params, a.Secrets = nil, nil
	b := sampleDispatch()
	b.Params, b.Secrets = map[string]string{}, map[string]string{}
	if !bytes.Equal(a.SigningBytes(), b.SigningBytes()) {
		t.Error("nil and empty maps should encode identically")
	}
}
