package proto

import (
	"bytes"
	"encoding/binary"
	"sort"
)

// SigningBytes returns a deterministic encoding of the dispatch for signing and
// verification. The Signature field itself is excluded.
//
// Every field is length-prefixed so no value can be crafted to look like a field
// boundary (e.g. a param value containing a separator). Map keys are sorted, so the
// same dispatch always produces the same bytes on server and runner.
//
// The artifact's *content* is covered through its SHA-256: the runner verifies the
// bytes it received against that hash, so signing the hash is enough to pin the code.
func (d JobDispatch) SigningBytes() []byte {
	var b bytes.Buffer
	writeField(&b, []byte("arcatum-dispatch/"+Version))
	writeField(&b, []byte(d.RunID))
	writeField(&b, []byte(d.InstanceID))
	writeField(&b, []byte(d.Script))
	writeField(&b, []byte(d.Type))
	writeField(&b, []byte(d.Artifact.Filename))
	writeField(&b, []byte(d.Artifact.SHA256))
	writeUint64(&b, uint64(d.TimeoutSec))
	writeField(&b, []byte(d.Capture))
	writeMap(&b, d.Params)
	writeMap(&b, d.Secrets)
	return b.Bytes()
}

func writeField(b *bytes.Buffer, data []byte) {
	writeUint64(b, uint64(len(data)))
	b.Write(data)
}

func writeUint64(b *bytes.Buffer, n uint64) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], n)
	b.Write(buf[:])
}

// writeMap encodes a map with sorted keys so the result is order-independent.
func writeMap(b *bytes.Buffer, m map[string]string) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	writeUint64(b, uint64(len(keys)))
	for _, k := range keys {
		writeField(b, []byte(k))
		writeField(b, []byte(m[k]))
	}
}
