// ABOUTME: Tests for downloadRegistry and sharedFilesStore encrypted persistence.
// ABOUTME: Covers round-trip, peer isolation, corruption recovery, and nil-receiver safety.
package common

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

var testMasterKey = []byte("test-master-key-for-unit-tests!!")

func TestDownloadRegistry_RegisterAndForPeer(t *testing.T) {
	dir := t.TempDir()
	r := newDownloadRegistry(dir, testMasterKey)
	tag := r.peerTag("alice-fp", testMasterKey)
	otherTag := r.peerTag("bob-fp", testMasterKey)

	r.Register("/tmp/a.kdbitmap", tag)
	r.Register("/tmp/b.kdbitmap", tag)
	r.Register("/tmp/c.kdbitmap", otherTag)

	got := r.ForPeer(tag)
	require.Len(t, got, 2)
	require.Equal(t, 3, r.Count())
}

func TestDownloadRegistry_Unregister(t *testing.T) {
	dir := t.TempDir()
	r := newDownloadRegistry(dir, testMasterKey)
	tag := r.peerTag("alice-fp", testMasterKey)

	r.Register("/tmp/a.kdbitmap", tag)
	r.Register("/tmp/b.kdbitmap", tag)
	r.Unregister("/tmp/a.kdbitmap")

	got := r.ForPeer(tag)
	require.Len(t, got, 1)
}

func TestDownloadRegistry_PersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	r := newDownloadRegistry(dir, testMasterKey)
	tag := r.peerTag("alice-fp", testMasterKey)

	r.Register("/tmp/a.kdbitmap", tag)
	r.Register("/tmp/b.kdbitmap", tag)

	r2 := newDownloadRegistry(dir, testMasterKey)
	got := r2.ForPeer(tag)
	require.Len(t, got, 2)
}

func TestDownloadRegistry_CorruptFileRecovery(t *testing.T) {
	dir := t.TempDir()
	r := newDownloadRegistry(dir, testMasterKey)
	tag := r.peerTag("alice-fp", testMasterKey)
	r.Register("/tmp/a.kdbitmap", tag)

	regPath := filepath.Join(dir, ".kd_registry")
	require.NoError(t, os.WriteFile(regPath, []byte("garbage"), 0600))

	r2 := newDownloadRegistry(dir, testMasterKey)
	require.Equal(t, 0, r2.Count(), "corrupt file should yield empty registry")
	_, err := os.Stat(regPath)
	require.True(t, os.IsNotExist(err), "corrupt file should be deleted on load")
}

func TestDownloadRegistry_EmptyDeletesFile(t *testing.T) {
	dir := t.TempDir()
	r := newDownloadRegistry(dir, testMasterKey)
	tag := r.peerTag("alice-fp", testMasterKey)
	r.Register("/tmp/a.kdbitmap", tag)

	regPath := filepath.Join(dir, ".kd_registry")
	_, err := os.Stat(regPath)
	require.NoError(t, err, "registry file should exist after Register")

	r.Unregister("/tmp/a.kdbitmap")
	_, err = os.Stat(regPath)
	require.True(t, os.IsNotExist(err), "registry file should be deleted when empty")
}

func TestDownloadRegistry_MemoryOnlyMode(t *testing.T) {
	r := newDownloadRegistry("", nil)
	tag := r.peerTag("alice-fp", testMasterKey)
	r.Register("/tmp/a.kdbitmap", tag)

	require.Equal(t, 1, r.Count())
}

func TestDownloadRegistry_PeerTagDeterministic(t *testing.T) {
	r := newDownloadRegistry("", nil)
	t1 := r.peerTag("fp-abc", testMasterKey)
	t2 := r.peerTag("fp-abc", testMasterKey)
	require.Equal(t, t1, t2, "peerTag should be deterministic for same inputs")

	t3 := r.peerTag("fp-xyz", testMasterKey)
	require.NotEqual(t, t1, t3, "peerTag should differ for different fingerprints")
}

func TestSharedFilesStore_SaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := newSharedFilesStore(dir, testMasterKey)
	require.NotNil(t, s, "store should not be nil with valid inputs")

	tag := [16]byte{1, 2, 3}
	entries := []sharedEntry{
		{PeerTag: tag, Path: "/tmp/photo.jpg", Size: 1024, ModTime: 99},
		{PeerTag: tag, Path: "/tmp/doc.pdf", Size: 2048, ModTime: 100},
	}
	s.Save(entries)

	got := s.Load()
	require.Len(t, got, 2)
	require.Equal(t, "/tmp/photo.jpg", got[0].Path)
	require.Equal(t, uint64(1024), got[0].Size)
	require.Equal(t, "/tmp/doc.pdf", got[1].Path)
	require.Equal(t, uint64(2048), got[1].Size)
}

func TestSharedFilesStore_Clear(t *testing.T) {
	dir := t.TempDir()
	s := newSharedFilesStore(dir, testMasterKey)
	s.Save([]sharedEntry{{Path: "/tmp/a.txt", Size: 1}})

	storePath := filepath.Join(dir, ".kd_shared")
	_, err := os.Stat(storePath)
	require.NoError(t, err, "store file should exist")

	s.Clear()
	_, err = os.Stat(storePath)
	require.True(t, os.IsNotExist(err), "store file should be deleted after Clear")
}

func TestSharedFilesStore_SaveEmptyDeletesFile(t *testing.T) {
	dir := t.TempDir()
	s := newSharedFilesStore(dir, testMasterKey)
	s.Save([]sharedEntry{{Path: "/tmp/a.txt", Size: 1}})

	s.Save(nil)

	storePath := filepath.Join(dir, ".kd_shared")
	_, err := os.Stat(storePath)
	require.True(t, os.IsNotExist(err), "empty save should delete store file")
}

func TestSharedFilesStore_CorruptFileReturnsNil(t *testing.T) {
	dir := t.TempDir()
	s := newSharedFilesStore(dir, testMasterKey)

	storePath := filepath.Join(dir, ".kd_shared")
	require.NoError(t, os.WriteFile(storePath, []byte("not-encrypted"), 0600))

	got := s.Load()
	require.Nil(t, got, "corrupt file should return nil")
	_, err := os.Stat(storePath)
	require.True(t, os.IsNotExist(err), "corrupt file should be deleted")
}

func TestSharedFilesStore_NilReceiverSafe(t *testing.T) {
	var s *sharedFilesStore
	s.Save([]sharedEntry{{Path: "/tmp/a.txt"}})
	got := s.Load()
	s.Clear()

	require.Nil(t, got, "nil receiver Load should return nil")
}

func TestSharedFilesStore_NilInputsReturnsNil(t *testing.T) {
	require.Nil(t, newSharedFilesStore("", testMasterKey), "empty configDir should return nil")
	require.Nil(t, newSharedFilesStore(t.TempDir(), nil), "nil masterKey should return nil")
}

func TestSharedFilesStore_PeerIsolation(t *testing.T) {
	dir := t.TempDir()
	s := newSharedFilesStore(dir, testMasterKey)

	tagA := [16]byte{1}
	tagB := [16]byte{2}
	entries := []sharedEntry{
		{PeerTag: tagA, Path: "/tmp/for-alice.txt"},
		{PeerTag: tagB, Path: "/tmp/for-bob.txt"},
	}
	s.Save(entries)

	got := s.Load()
	var aliceCount, bobCount int
	for _, e := range got {
		if e.PeerTag == tagA {
			aliceCount++
		}
		if e.PeerTag == tagB {
			bobCount++
		}
	}
	require.Equal(t, 1, aliceCount)
	require.Equal(t, 1, bobCount)
}

func TestEncryptDecryptAESGCM_RoundTrip(t *testing.T) {
	key := make([]byte, 32)
	copy(key, []byte("32-byte-key-for-aes-256-testing!"))
	plaintext := []byte("hello world")

	ct, err := encryptAESGCM(key, plaintext)
	require.NoError(t, err)

	pt, err := decryptAESGCM(key, ct)
	require.NoError(t, err)
	require.Equal(t, "hello world", string(pt))
}

func TestDecryptAESGCM_WrongKey(t *testing.T) {
	key1 := make([]byte, 32)
	key2 := make([]byte, 32)
	copy(key1, []byte("key-one-for-encrypt-test-padding"))
	copy(key2, []byte("key-two-for-decrypt-test-padding"))

	ct, err := encryptAESGCM(key1, []byte("secret"))
	require.NoError(t, err)

	_, err = decryptAESGCM(key2, ct)
	require.Error(t, err, "decrypt with wrong key should fail")
}

func TestDecryptAESGCM_TooShort(t *testing.T) {
	key := make([]byte, 32)
	copy(key, []byte("32-byte-key-for-short-ct-test!!!"))

	_, err := decryptAESGCM(key, []byte("short"))
	require.Error(t, err, "decrypt of too-short ciphertext should fail")
}
