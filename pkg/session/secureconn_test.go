// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

package session

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSecureConn_Integrity(t *testing.T) {
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = 0x42
	}

	// 1. Test normal sequential read/write
	var buf bytes.Buffer
	// Match prefixes for testing (Reader uses Inbound, so Writer must too)
	writer := NewSecureWriterWithPrefix(&buf, kek, NoncePrefixInbound)
	reader := NewSecureReader(&buf, kek)

	msg1 := []byte("hello")
	msg2 := []byte("world")

	_, err := writer.Write(msg1)
	require.NoError(t, err)
	_, err = writer.Write(msg2)
	require.NoError(t, err)

	got1, err := reader.Read()
	require.NoError(t, err)
	assert.Equal(t, msg1, got1)

	got2, err := reader.Read()
	require.NoError(t, err)
	assert.Equal(t, msg2, got2)

	// 2. Test reordering attack (swap two messages)
	buf.Reset()
	
	// We need them to be from the same stream sequence
	wStream := NewSecureWriterWithPrefix(&buf, kek, NoncePrefixInbound)
	_, _ = wStream.Write(msg1) // sequence 1
	ct1 := make([]byte, buf.Len())
	copy(ct1, buf.Bytes())
	
	buf.Reset()
	_, _ = wStream.Write(msg2) // sequence 2
	ct2 := make([]byte, buf.Len())
	copy(ct2, buf.Bytes())

	// Now swap them in the final buffer
	buf.Reset()
	buf.Write(ct2)
	buf.Write(ct1)

	rStream := NewSecureReader(&buf, kek)
	_, err = rStream.Read()
	assert.Error(t, err, "Should fail because ct2 (seq 2) was provided when seq 1 was expected")
	assert.Contains(t, err.Error(), "decryption failed")
}

func TestSecureConn_Replay(t *testing.T) {
	kek := make([]byte, 32)
	var buf bytes.Buffer
	
	writer := NewSecureWriterWithPrefix(&buf, kek, NoncePrefixInbound)
	msg := []byte("replayed message")
	_, err := writer.Write(msg)
	require.NoError(t, err)
	
	ct := buf.Bytes()
	
	// Simulate replay: send the same ciphertext twice
	buf.Reset()
	buf.Write(ct)
	buf.Write(ct)
	
	reader := NewSecureReader(&buf, kek)
	_, err = reader.Read()
	require.NoError(t, err, "First read should succeed")
	
	_, err = reader.Read()
	assert.Error(t, err, "Second read (replay) should fail")
}
