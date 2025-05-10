package session

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"

	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
)

const lengthHeaderSize = 4 // uint32 prefix

// SecureWriter encrypts messages and writes them to an underlying writer.
type SecureWriter struct {
	w   io.Writer
	kek []byte
}

func NewSecureWriter(w io.Writer, kek []byte) *SecureWriter {
	return &SecureWriter{w: w, kek: kek}
}

func (s *SecureWriter) Write(p []byte) (int, error) {
	encrypted, err := kbc.Encrypt(s.kek, p)
	if err != nil {
		return 0, fmt.Errorf("encryption failed: %w", err)
	}

	length := uint32(len(encrypted))
	head := make([]byte, lengthHeaderSize)
	binary.BigEndian.PutUint32(head, length)

	// Write [length][encrypted]
	if _, err := s.w.Write(head); err != nil {
		return 0, fmt.Errorf("write length failed: %w", err)
	}
	if _, err := s.w.Write(encrypted); err != nil {
		return 0, fmt.Errorf("write data failed: %w", err)
	}

	return len(p), nil // number of plaintext bytes consumed
}

// SecureReader reads encrypted messages and decrypts them.
type SecureReader struct {
	r   io.Reader
	kek []byte
}

func NewSecureReader(r io.Reader, kek []byte) *SecureReader {
	return &SecureReader{r: r, kek: kek}
}

func (s *SecureReader) Read() ([]byte, error) {
	head := make([]byte, lengthHeaderSize)
	if _, err := io.ReadFull(s.r, head); err != nil {
		return nil, fmt.Errorf("read length failed: %w", err)
	}
	length := binary.BigEndian.Uint32(head)

	encrypted := make([]byte, length)
	if _, err := io.ReadFull(s.r, encrypted); err != nil {
		return nil, fmt.Errorf("read encrypted block failed: %w", err)
	}

	plaintext, err := kbc.Decrypt(s.kek, encrypted)
	if err != nil {
		return nil, fmt.Errorf("decryption failed: %w", err)
	}

	return plaintext, nil
}

// SecureConn wraps a net.Conn with separate inbound/outbound encryption.
type SecureConn struct {
	conn net.Conn
	r    *SecureReader
	w    *SecureWriter
}

func NewSecureConn(conn net.Conn, kek []byte) *SecureConn {
	return &SecureConn{
		conn: conn,
		r:    NewSecureReader(conn, kek),
		w:    NewSecureWriter(conn, kek),
	}
}

// ReadMessage reads and decrypts a full message.
func (s *SecureConn) ReadMessage() ([]byte, error) {
	return s.r.Read()
}

// WriteMessage encrypts and writes a full message.
func (s *SecureConn) WriteMessage(msg []byte) error {
	_, err := s.w.Write(msg)
	return err
}

// Close closes the underlying connection.
func (s *SecureConn) Close() error {
	return s.conn.Close()
}

// RemoteAddr returns the remote network address.
func (s *SecureConn) RemoteAddr() net.Addr {
	return s.conn.RemoteAddr()
}

// LocalAddr returns the local network address.
func (s *SecureConn) LocalAddr() net.Addr {
	return s.conn.LocalAddr()
}
