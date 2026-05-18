// ABOUTME: Tests for downloadRegistry and sharedFilesStore encrypted persistence.
// ABOUTME: Covers round-trip, peer isolation, corruption recovery, and nil-receiver safety.
package common

import (
	"os"
	"path/filepath"
	"testing"
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
	if len(got) != 2 {
		t.Fatalf("ForPeer(alice) = %d entries, want 2", len(got))
	}
	if r.Count() != 3 {
		t.Fatalf("Count() = %d, want 3", r.Count())
	}
}

func TestDownloadRegistry_Unregister(t *testing.T) {
	dir := t.TempDir()
	r := newDownloadRegistry(dir, testMasterKey)
	tag := r.peerTag("alice-fp", testMasterKey)

	r.Register("/tmp/a.kdbitmap", tag)
	r.Register("/tmp/b.kdbitmap", tag)
	r.Unregister("/tmp/a.kdbitmap")

	got := r.ForPeer(tag)
	if len(got) != 1 {
		t.Fatalf("ForPeer after Unregister = %d, want 1", len(got))
	}
}

func TestDownloadRegistry_PersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	r := newDownloadRegistry(dir, testMasterKey)
	tag := r.peerTag("alice-fp", testMasterKey)

	r.Register("/tmp/a.kdbitmap", tag)
	r.Register("/tmp/b.kdbitmap", tag)

	r2 := newDownloadRegistry(dir, testMasterKey)
	got := r2.ForPeer(tag)
	if len(got) != 2 {
		t.Fatalf("ForPeer after reload = %d, want 2", len(got))
	}
}

func TestDownloadRegistry_CorruptFileRecovery(t *testing.T) {
	dir := t.TempDir()
	r := newDownloadRegistry(dir, testMasterKey)
	tag := r.peerTag("alice-fp", testMasterKey)
	r.Register("/tmp/a.kdbitmap", tag)

	regPath := filepath.Join(dir, ".kd_registry")
	if err := os.WriteFile(regPath, []byte("garbage"), 0600); err != nil {
		t.Fatal(err)
	}

	r2 := newDownloadRegistry(dir, testMasterKey)
	if r2.Count() != 0 {
		t.Fatalf("corrupt file should yield empty registry, got %d", r2.Count())
	}
	if _, err := os.Stat(regPath); !os.IsNotExist(err) {
		t.Fatal("corrupt file should be deleted on load")
	}
}

func TestDownloadRegistry_EmptyDeletesFile(t *testing.T) {
	dir := t.TempDir()
	r := newDownloadRegistry(dir, testMasterKey)
	tag := r.peerTag("alice-fp", testMasterKey)
	r.Register("/tmp/a.kdbitmap", tag)

	regPath := filepath.Join(dir, ".kd_registry")
	if _, err := os.Stat(regPath); err != nil {
		t.Fatalf("registry file should exist after Register: %v", err)
	}

	r.Unregister("/tmp/a.kdbitmap")
	if _, err := os.Stat(regPath); !os.IsNotExist(err) {
		t.Fatal("registry file should be deleted when empty")
	}
}

func TestDownloadRegistry_MemoryOnlyMode(t *testing.T) {
	r := newDownloadRegistry("", nil)
	tag := r.peerTag("alice-fp", testMasterKey)
	r.Register("/tmp/a.kdbitmap", tag)

	if r.Count() != 1 {
		t.Fatalf("memory-only Count() = %d, want 1", r.Count())
	}
}

func TestDownloadRegistry_PeerTagDeterministic(t *testing.T) {
	r := newDownloadRegistry("", nil)
	t1 := r.peerTag("fp-abc", testMasterKey)
	t2 := r.peerTag("fp-abc", testMasterKey)
	if t1 != t2 {
		t.Fatal("peerTag should be deterministic for same inputs")
	}

	t3 := r.peerTag("fp-xyz", testMasterKey)
	if t1 == t3 {
		t.Fatal("peerTag should differ for different fingerprints")
	}
}

func TestSharedFilesStore_SaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := newSharedFilesStore(dir, testMasterKey)
	if s == nil {
		t.Fatal("store should not be nil with valid inputs")
	}

	tag := [16]byte{1, 2, 3}
	entries := []sharedEntry{
		{PeerTag: tag, Path: "/tmp/photo.jpg", Size: 1024, ModTime: 99},
		{PeerTag: tag, Path: "/tmp/doc.pdf", Size: 2048, ModTime: 100},
	}
	s.Save(entries)

	got := s.Load()
	if len(got) != 2 {
		t.Fatalf("Load() = %d entries, want 2", len(got))
	}
	if got[0].Path != "/tmp/photo.jpg" || got[0].Size != 1024 {
		t.Fatalf("entry 0 mismatch: %+v", got[0])
	}
	if got[1].Path != "/tmp/doc.pdf" || got[1].Size != 2048 {
		t.Fatalf("entry 1 mismatch: %+v", got[1])
	}
}

func TestSharedFilesStore_Clear(t *testing.T) {
	dir := t.TempDir()
	s := newSharedFilesStore(dir, testMasterKey)
	s.Save([]sharedEntry{{Path: "/tmp/a.txt", Size: 1}})

	storePath := filepath.Join(dir, ".kd_shared")
	if _, err := os.Stat(storePath); err != nil {
		t.Fatalf("store file should exist: %v", err)
	}

	s.Clear()
	if _, err := os.Stat(storePath); !os.IsNotExist(err) {
		t.Fatal("store file should be deleted after Clear")
	}
}

func TestSharedFilesStore_SaveEmptyDeletesFile(t *testing.T) {
	dir := t.TempDir()
	s := newSharedFilesStore(dir, testMasterKey)
	s.Save([]sharedEntry{{Path: "/tmp/a.txt", Size: 1}})

	s.Save(nil)

	storePath := filepath.Join(dir, ".kd_shared")
	if _, err := os.Stat(storePath); !os.IsNotExist(err) {
		t.Fatal("empty save should delete store file")
	}
}

func TestSharedFilesStore_CorruptFileReturnsNil(t *testing.T) {
	dir := t.TempDir()
	s := newSharedFilesStore(dir, testMasterKey)

	storePath := filepath.Join(dir, ".kd_shared")
	if err := os.WriteFile(storePath, []byte("not-encrypted"), 0600); err != nil {
		t.Fatal(err)
	}

	got := s.Load()
	if got != nil {
		t.Fatalf("corrupt file should return nil, got %d entries", len(got))
	}
	if _, err := os.Stat(storePath); !os.IsNotExist(err) {
		t.Fatal("corrupt file should be deleted")
	}
}

func TestSharedFilesStore_NilReceiverSafe(t *testing.T) {
	var s *sharedFilesStore
	s.Save([]sharedEntry{{Path: "/tmp/a.txt"}})
	got := s.Load()
	s.Clear()

	if got != nil {
		t.Fatal("nil receiver Load should return nil")
	}
}

func TestSharedFilesStore_NilInputsReturnsNil(t *testing.T) {
	if s := newSharedFilesStore("", testMasterKey); s != nil {
		t.Fatal("empty configDir should return nil")
	}
	if s := newSharedFilesStore(t.TempDir(), nil); s != nil {
		t.Fatal("nil masterKey should return nil")
	}
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
	if aliceCount != 1 || bobCount != 1 {
		t.Fatalf("peer isolation: alice=%d bob=%d, want 1 each", aliceCount, bobCount)
	}
}

func TestEncryptDecryptAESGCM_RoundTrip(t *testing.T) {
	key := make([]byte, 32)
	copy(key, []byte("32-byte-key-for-aes-256-testing!"))
	plaintext := []byte("hello world")

	ct, err := encryptAESGCM(key, plaintext)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	pt, err := decryptAESGCM(key, ct)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if string(pt) != "hello world" {
		t.Fatalf("round-trip mismatch: %q", pt)
	}
}

func TestDecryptAESGCM_WrongKey(t *testing.T) {
	key1 := make([]byte, 32)
	key2 := make([]byte, 32)
	copy(key1, []byte("key-one-for-encrypt-test-padding"))
	copy(key2, []byte("key-two-for-decrypt-test-padding"))

	ct, err := encryptAESGCM(key1, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}

	_, err = decryptAESGCM(key2, ct)
	if err == nil {
		t.Fatal("decrypt with wrong key should fail")
	}
}

func TestDecryptAESGCM_TooShort(t *testing.T) {
	key := make([]byte, 32)
	copy(key, []byte("32-byte-key-for-short-ct-test!!!"))

	_, err := decryptAESGCM(key, []byte("short"))
	if err == nil {
		t.Fatal("decrypt of too-short ciphertext should fail")
	}
}
