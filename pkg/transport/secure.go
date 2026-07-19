package transport

import (
	"context"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/mlkem"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

// Role identifies the two ends of the PQC handshake so they derive matching keys.
type Role int

const (
	RoleClient Role = iota
	RoleServer
)

const maxSecureFrame = 20 << 20 // 20 MiB, matches KeibiDrop's cap

// Secure runs a hybrid post-quantum handshake (ML-KEM-1024 + X25519) over conn and
// returns an encrypted net.Conn (ChaCha20-Poly1305, length-prefixed AEAD frames).
//
// This is the post-quantum guarantee the QUIC channel needs on top of it: QUIC
// mandates TLS 1.3, which is *classical* — a harvest-now-decrypt-later adversary
// breaks it once a quantum computer exists. This handshake is PQ-hybrid, so the
// bytes stay confidential. It uses the same primitives as KeibiDrop's session layer
// (crypto/mlkem + crypto/ecdh + HKDF + ChaCha20-Poly1305), so the real handshake
// drops into this seam in Phase 3.
func Secure(_ context.Context, conn net.Conn, role Role) (net.Conn, error) {
	secret, err := pqHandshake(conn, role)
	if err != nil {
		return nil, fmt.Errorf("pq handshake: %w", err)
	}
	// Directional keys so the two directions never share a nonce space.
	c2s := deriveKey(secret, "kd-quic client->server")
	s2c := deriveKey(secret, "kd-quic server->client")
	wkey, rkey := c2s, s2c
	if role == RoleServer {
		wkey, rkey = s2c, c2s
	}
	return newSecureConn(conn, wkey, rkey)
}

// pqHandshake performs an ML-KEM-1024 + X25519 hybrid key exchange over conn and
// returns the combined shared secret (both halves concatenated, then HKDF'd by the
// caller). A quantum computer must break BOTH ML-KEM and X25519 to recover it.
func pqHandshake(conn net.Conn, role Role) ([]byte, error) {
	// --- X25519 (classical half): both sides send an ephemeral public key ---
	xPriv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := writeFrame(conn, xPriv.PublicKey().Bytes()); err != nil {
		return nil, err
	}
	peerXBytes, err := readFrame(conn)
	if err != nil {
		return nil, err
	}
	peerX, err := ecdh.X25519().NewPublicKey(peerXBytes)
	if err != nil {
		return nil, err
	}
	xShared, err := xPriv.ECDH(peerX)
	if err != nil {
		return nil, err
	}

	// --- ML-KEM-1024 (post-quantum half): client offers a key, server encapsulates ---
	var kShared []byte
	switch role {
	case RoleClient:
		dk, err := mlkem.GenerateKey1024()
		if err != nil {
			return nil, err
		}
		if err := writeFrame(conn, dk.EncapsulationKey().Bytes()); err != nil {
			return nil, err
		}
		ct, err := readFrame(conn)
		if err != nil {
			return nil, err
		}
		kShared, err = dk.Decapsulate(ct)
		if err != nil {
			return nil, err
		}
	case RoleServer:
		ekBytes, err := readFrame(conn)
		if err != nil {
			return nil, err
		}
		ek, err := mlkem.NewEncapsulationKey1024(ekBytes)
		if err != nil {
			return nil, err
		}
		shared, ct := ek.Encapsulate()
		kShared = shared
		if err := writeFrame(conn, ct); err != nil {
			return nil, err
		}
	}

	return append(append(make([]byte, 0, len(xShared)+len(kShared)), xShared...), kShared...), nil
}

func deriveKey(secret []byte, label string) []byte {
	r := hkdf.New(sha256.New, secret, nil, []byte(label))
	key := make([]byte, chacha20poly1305.KeySize)
	if _, err := io.ReadFull(r, key); err != nil {
		panic(err) // HKDF over 32 bytes cannot fail
	}
	return key
}

// writeFrame / readFrame: length-prefixed handshake messages (4-byte big-endian len).
func writeFrame(w io.Writer, b []byte) error {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(b)
	return err
}

func readFrame(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxSecureFrame {
		return nil, fmt.Errorf("handshake frame too large: %d", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return b, nil
}

// secureConn is an AEAD-framed net.Conn: [4B len][12B nonce][ciphertext+tag] per
// message, ChaCha20-Poly1305, one key + counter-nonce per direction. Same wire
// shape as KeibiDrop's SecureConn, including the leftover buffering that lets it
// serve gRPC's arbitrary Read sizes from message-framed ciphertext.
type secureConn struct {
	net.Conn
	waead, raead cipher.AEAD
	wmu          sync.Mutex
	rmu          sync.Mutex
	wctr, rctr   uint64
	leftover     []byte
}

func newSecureConn(inner net.Conn, wkey, rkey []byte) (net.Conn, error) {
	waead, err := chacha20poly1305.New(wkey)
	if err != nil {
		return nil, err
	}
	raead, err := chacha20poly1305.New(rkey)
	if err != nil {
		return nil, err
	}
	return &secureConn{Conn: inner, waead: waead, raead: raead}, nil
}

func (c *secureConn) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	var nonce [chacha20poly1305.NonceSize]byte
	binary.BigEndian.PutUint64(nonce[chacha20poly1305.NonceSize-8:], c.wctr)
	c.wctr++

	bodyLen := chacha20poly1305.NonceSize + len(p) + c.waead.Overhead()
	buf := make([]byte, 4+bodyLen)
	binary.BigEndian.PutUint32(buf[:4], uint32(bodyLen))
	copy(buf[4:], nonce[:])
	c.waead.Seal(buf[4+chacha20poly1305.NonceSize:4+chacha20poly1305.NonceSize], nonce[:], p, nil)

	if _, err := c.Conn.Write(buf); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *secureConn) Read(p []byte) (int, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	if len(c.leftover) > 0 {
		n := copy(p, c.leftover)
		c.leftover = c.leftover[n:]
		return n, nil
	}

	var hdr [4]byte
	if _, err := io.ReadFull(c.Conn, hdr[:]); err != nil {
		return 0, err
	}
	bodyLen := binary.BigEndian.Uint32(hdr[:])
	if bodyLen < uint32(chacha20poly1305.NonceSize+c.raead.Overhead()) || bodyLen > maxSecureFrame {
		return 0, fmt.Errorf("secureConn: bad frame length %d", bodyLen)
	}
	body := make([]byte, bodyLen)
	if _, err := io.ReadFull(c.Conn, body); err != nil {
		return 0, err
	}
	nonce, ct := body[:chacha20poly1305.NonceSize], body[chacha20poly1305.NonceSize:]
	plain, err := c.raead.Open(ct[:0], nonce, ct, nil) // decrypt in place
	if err != nil {
		return 0, fmt.Errorf("secureConn: decrypt: %w", err)
	}
	n := copy(p, plain)
	if n < len(plain) {
		c.leftover = plain[n:]
	}
	return n, nil
}
