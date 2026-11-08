package bundle

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"
)

// CheckPack checks the pack in a bundle: it reads the bundle's header, then
// the whole pack, and compares the pack's trailing checksum with the checksum
// of everything before it. git bundle verify reads only the header, so it
// accepts a bundle whose pack is damaged or cut short.
//
// It reads v2 bundles and v3 bundles, whose pack checksum is SHA-1 or, with
// "@object-format=sha256", SHA-256.
func CheckPack(r io.Reader) error {
	br := bufio.NewReader(r)
	newHash, err := readHeader(br)
	if err != nil {
		return err
	}

	h := newHash()
	var pack [12]byte // "PACK", the version, the number of objects
	if _, err := io.ReadFull(br, pack[:]); err != nil {
		return fmt.Errorf("pack header: %w", unexpected(err))
	}
	if string(pack[:4]) != "PACK" {
		return errors.New("no pack after the bundle header")
	}
	if v := pack[7]; pack[4] != 0 || pack[5] != 0 || pack[6] != 0 || (v != 2 && v != 3) {
		return fmt.Errorf("unknown pack version %x", pack[4:8])
	}
	h.Write(pack[:])

	// The last size bytes are the checksum, so they mustn't be hashed: keep
	// up to size unhashed bytes at the start of buf until the next read.
	size := h.Size()
	buf := make([]byte, size+64<<10)
	n := 0
	for {
		m, err := br.Read(buf[n:])
		n += m
		if n > size {
			h.Write(buf[:n-size])
			n = copy(buf, buf[n-size:n])
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	if n < size {
		return errors.New("pack is cut short: no room for its checksum")
	}
	if !bytes.Equal(h.Sum(nil), buf[:size]) {
		return errors.New("pack checksum mismatch: the pack is damaged or cut short")
	}
	return nil
}

// readHeader reads a bundle's header, up to and including the blank line
// before the pack, and returns the pack's checksum function.
func readHeader(br *bufio.Reader) (func() hash.Hash, error) {
	first, err := br.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	v3 := first == "# v3 git bundle\n"
	if first != "# v2 git bundle\n" && !v3 {
		return nil, errors.New("not a v2 or v3 git bundle")
	}
	newHash := sha1.New
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("bundle header: %w", unexpected(err))
		}
		if line == "\n" {
			return newHash, nil
		}
		// v3 capabilities. Only the object format matters here.
		if format, ok := strings.CutPrefix(line, "@object-format="); ok && v3 {
			switch strings.TrimSuffix(format, "\n") {
			case "sha1":
				newHash = sha1.New
			case "sha256":
				newHash = sha256.New
			default:
				return nil, fmt.Errorf("unknown object format %q", strings.TrimSpace(format))
			}
		}
	}
}

// unexpected turns io.EOF into io.ErrUnexpectedEOF: the bundle ended too early.
func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}
