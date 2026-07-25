package runner

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"

	"arcatum/pkg/crypto"
	"arcatum/pkg/proto"
)

// signedDispatch returns a dispatch signed by a fresh key, plus a verifier for it and
// one for an unrelated key.
func signedDispatch(t *testing.T) (proto.JobDispatch, crypto.Verifier, crypto.Verifier) {
	t.Helper()
	dir := t.TempDir()

	newKeypair := func(name string) (*crypto.Ed25519Signer, crypto.Verifier) {
		privPEM, pubPEM, err := crypto.GenerateSigningKey()
		if err != nil {
			t.Fatalf("GenerateSigningKey: %v", err)
		}
		privPath := filepath.Join(dir, name+".key")
		pubPath := filepath.Join(dir, name+".pub")
		if err := os.WriteFile(privPath, privPEM, 0o600); err != nil {
			t.Fatalf("write key: %v", err)
		}
		if err := os.WriteFile(pubPath, pubPEM, 0o600); err != nil {
			t.Fatalf("write pub: %v", err)
		}
		signer, err := crypto.LoadSigner(privPath)
		if err != nil {
			t.Fatalf("LoadSigner: %v", err)
		}
		verifier, err := crypto.LoadVerifier(pubPath)
		if err != nil {
			t.Fatalf("LoadVerifier: %v", err)
		}
		return signer, verifier
	}

	signer, verifier := newKeypair("server")
	_, foreignVerifier := newKeypair("impostor")

	d := proto.JobDispatch{
		RunID:      "run-1",
		InstanceID: "hello-demo",
		Script:     "hello",
		Type:       proto.TypeBash,
		Artifact:   proto.Artifact{Filename: "hello.sh", SHA256: "abc", Content: []byte("echo hi\n")},
		Params:     map[string]string{"name": "demo"},
		TimeoutSec: 60,
		Capture:    "stream",
	}
	sig, err := signer.Sign(d.SigningBytes())
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	d.Signature = sig
	return d, verifier, foreignVerifier
}

func testAgent(verifier crypto.Verifier) *Agent {
	return &Agent{log: log.New(io.Discard, "", 0), verifier: verifier}
}

func TestVerifyDispatchAcceptsValidSignature(t *testing.T) {
	d, verifier, _ := signedDispatch(t)
	if err := testAgent(verifier).verifyDispatch(d); err != nil {
		t.Errorf("verifyDispatch = %v, want nil", err)
	}
}

// The whole point of signing: a job that was altered in transit must not run.
func TestVerifyDispatchRejectsTamperedDispatch(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*proto.JobDispatch)
	}{
		{"swapped script", func(d *proto.JobDispatch) { d.Script = "evil" }},
		{"swapped artifact hash", func(d *proto.JobDispatch) { d.Artifact.SHA256 = "deadbeef" }},
		{"added param", func(d *proto.JobDispatch) { d.Params["extra"] = "x" }},
		{"changed run id", func(d *proto.JobDispatch) { d.RunID = "run-99" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, verifier, _ := signedDispatch(t)
			tc.mutate(&d)
			if err := testAgent(verifier).verifyDispatch(d); err == nil {
				t.Error("a tampered dispatch must be refused")
			}
		})
	}
}

// An impostor server holding no Arcatum signing key must not get code executed.
func TestVerifyDispatchRejectsForeignSigner(t *testing.T) {
	d, _, foreign := signedDispatch(t)
	if err := testAgent(foreign).verifyDispatch(d); err == nil {
		t.Error("a dispatch signed by an unknown key must be refused")
	}
}

func TestVerifyDispatchRejectsUnsignedDispatch(t *testing.T) {
	d, verifier, _ := signedDispatch(t)
	d.Signature = nil
	if err := testAgent(verifier).verifyDispatch(d); err == nil {
		t.Error("an unsigned dispatch must be refused when a verifier is configured")
	}
}

// Development mode has no verifier, so unsigned dispatches are allowed to run.
func TestVerifyDispatchDevModeAllowsUnsigned(t *testing.T) {
	d, _, _ := signedDispatch(t)
	d.Signature = nil
	if err := testAgent(nil).verifyDispatch(d); err != nil {
		t.Errorf("verifyDispatch (no verifier) = %v, want nil", err)
	}
}
