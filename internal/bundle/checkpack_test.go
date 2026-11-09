package bundle

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"math/rand/v2"
	"strings"
	"testing"
	"testing/iotest"
)

const (
	sha1ID   = "7bc1206d63a3e726b7763a32323e27cb4c7ff0cf"
	sha256ID = "cc44c5287f3130f363afc0970108b63355aa7b260755e41ab124019953b4f140"
)

// pack returns a pack with a valid header and checksum around body.
func pack(newHash func() hash.Hash, version byte, body []byte) []byte {
	p := append([]byte("PACK\x00\x00\x00"), version, 0, 0, 0, 1)
	p = append(p, body...)
	h := newHash()
	h.Write(p)
	return h.Sum(p)
}

func v2Bundle(body []byte) []byte {
	return append([]byte("# v2 git bundle\n"+sha1ID+" refs/heads/main\n\n"), pack(sha1.New, 2, body)...)
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.NewChaCha8([32]byte{}).Read(b)
	return b
}

func TestCheckPack(t *testing.T) {
	bundles := map[string][]byte{
		"v2":               v2Bundle(randomBytes(100)),
		"v2, pack v3":      append([]byte("# v2 git bundle\n"+sha1ID+" refs/heads/main\n\n"), pack(sha1.New, 3, randomBytes(100))...),
		"v2, prerequisite": append([]byte("# v2 git bundle\n-"+sha1ID+" parent\n"+sha1ID+" refs/heads/main\n\n"), pack(sha1.New, 2, nil)...),
		"v3, sha1":         append([]byte("# v3 git bundle\n@object-format=sha1\n"+sha1ID+" refs/heads/main\n\n"), pack(sha1.New, 2, randomBytes(100))...),
		"v3, sha256":       append([]byte("# v3 git bundle\n@object-format=sha256\n"+sha256ID+" refs/heads/main\n\n"), pack(sha256.New, 2, randomBytes(100))...),
		"v3, filter":       append([]byte("# v3 git bundle\n@filter=blob:none\n"+sha1ID+" refs/heads/main\n\n"), pack(sha1.New, 2, randomBytes(100))...),
	}
	// Sizes around the checksum's and the read buffer's.
	for _, n := range []int{0, 1, 19, 20, 21, 64<<10 - 1, 64 << 10, 64<<10 + 20, 300 << 10} {
		bundles[fmt.Sprintf("v2, %d byte body", n)] = v2Bundle(randomBytes(n))
	}
	readers := map[string]func(io.Reader) io.Reader{
		"plain":                   func(r io.Reader) io.Reader { return r },
		"one byte at a time":      iotest.OneByteReader,
		"half of each read":       iotest.HalfReader,
		"EOF with the last bytes": iotest.DataErrReader,
		"with a context":          func(r io.Reader) io.Reader { return ctxReader{t.Context(), r} },
	}
	for name, b := range bundles {
		for rname, wrap := range readers {
			if err := CheckPack(wrap(bytes.NewReader(b))); err != nil {
				t.Errorf("%s, read %s: %v", name, rname, err)
			}
		}
	}
}

func TestCheckPackErrors(t *testing.T) {
	good := v2Bundle(randomBytes(1000))
	flipped := bytes.Clone(good)
	flipped[len(flipped)-30] ^= 0xff
	flippedSum := bytes.Clone(good)
	flippedSum[len(flippedSum)-1] ^= 0x01

	tests := []struct {
		name    string
		bundle  []byte
		wantErr string
	}{
		{"empty", nil, "not a v2 or v3 git bundle"},
		{"v1", []byte("# v1 git bundle\n\n"), "not a v2 or v3 git bundle"},
		{"not a bundle", []byte("PACK...."), "not a v2 or v3 git bundle"},
		{"header cut short", []byte("# v2 git bundle\n" + sha1ID + " refs/heads/main\n"), "bundle header: unexpected EOF"},
		{"no pack", []byte("# v2 git bundle\n" + sha1ID + " refs/heads/main\n\n"), "pack header: unexpected EOF"},
		{"pack header cut short", []byte("# v2 git bundle\n\nPACK\x00\x00"), "pack header: unexpected EOF"},
		{"not a pack", []byte("# v2 git bundle\n\nKCAP\x00\x00\x00\x02\x00\x00\x00\x01"), "no pack after the bundle header"},
		{"pack version 4", append([]byte("# v2 git bundle\n\n"), pack(sha1.New, 4, nil)...), "unknown pack version 00000004"},
		{"unknown object format", []byte("# v3 git bundle\n@object-format=sha512\n\n"), `unknown object format "sha512"`},
		{"no room for the checksum", []byte("# v2 git bundle\n\nPACK\x00\x00\x00\x02\x00\x00\x00\x01short"), "pack is cut short"},
		{"flipped byte", flipped, "pack checksum mismatch"},
		{"flipped checksum", flippedSum, "pack checksum mismatch"},
		{"last byte missing", good[:len(good)-1], "pack checksum mismatch"},
		{"cut in half", good[:len(good)/2], "pack checksum mismatch"},
		{"extra byte", append(bytes.Clone(good), 0), "pack checksum mismatch"},
		// A SHA-256 pack in a bundle that doesn't say so.
		{"wrong object format", append([]byte("# v2 git bundle\n\n"), pack(sha256.New, 2, randomBytes(100))...), "pack checksum mismatch"},
	}
	for _, tt := range tests {
		err := CheckPack(bytes.NewReader(tt.bundle))
		if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("%s: error = %v, want it to contain %q", tt.name, err, tt.wantErr)
		}
	}
}

func TestCheckPackReadError(t *testing.T) {
	good := v2Bundle(randomBytes(1000))
	boom := errors.New("boom")
	r := io.MultiReader(bytes.NewReader(good[:len(good)/2]), iotest.ErrReader(boom))
	if err := CheckPack(r); !errors.Is(err, boom) {
		t.Errorf("error = %v, want %v", err, boom)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := CheckPack(ctxReader{ctx, bytes.NewReader(good)}); !errors.Is(err, context.Canceled) {
		t.Errorf("with a cancelled context: error = %v, want context.Canceled", err)
	}
}
