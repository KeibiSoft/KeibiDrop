package session

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"

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

	//#nosec:G115 // safe cast, no TCP stream frame will be 5GB.
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

	readBuf *bytes.Buffer
	done    bool
	closed  chan struct{}
}

func NewSecureConn(conn net.Conn, kek []byte) *SecureConn {
	return &SecureConn{
		conn:    conn,
		r:       NewSecureReader(conn, kek),
		w:       NewSecureWriter(conn, kek),
		readBuf: bytes.NewBuffer(nil),
		closed:  make(chan struct{}),
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
	if s.conn != nil {
		s.done = true
		close(s.closed)
		return s.conn.Close()
	}
	return nil
}

// RemoteAddr returns the remote network address.
func (s *SecureConn) RemoteAddr() net.Addr {
	return s.conn.RemoteAddr()
}

// LocalAddr returns the local network address.
func (s *SecureConn) LocalAddr() net.Addr {
	return s.conn.LocalAddr()
}

func (s *SecureConn) Read(p []byte) (int, error) {
	// Serve leftover decrypted data first
	if s.readBuf.Len() > 0 {
		return s.readBuf.Read(p)
	}

	// Nothing buffered, read and decrypt a full new message
	msg, err := s.r.Read()
	if err != nil {
		return 0, err
	}

	s.readBuf.Write(msg)
	return s.readBuf.Read(p)
}

func (s *SecureConn) Write(p []byte) (int, error) {
	n, err := s.w.Write(p)
	if err != nil {
		return 0, err
	}
	if n != len(p) {
		return n, io.ErrShortWrite
	}
	return n, nil
}

func (s *SecureConn) SetDeadline(t time.Time) error {
	return s.conn.SetDeadline(t)
}

func (s *SecureConn) SetReadDeadline(t time.Time) error {
	return s.conn.SetReadDeadline(t)
}

func (s *SecureConn) SetWriteDeadline(t time.Time) error {
	return s.conn.SetWriteDeadline(t)
}

func (s *SecureConn) Accept() (net.Conn, error) {
	if s.done {
		return nil, io.EOF
	}
	return s.conn, nil
}

func (l *SecureConn) Addr() net.Addr {
	return l.conn.LocalAddr()
}
