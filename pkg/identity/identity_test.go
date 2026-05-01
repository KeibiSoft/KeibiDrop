package identity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCreateAndLoad(t *testing.T) {
	dir := t.TempDir()

	id, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if id.Fingerprint == "" {
		t.Fatal("empty fingerprint")
	}
	if id.Keys == nil {
		t.Fatal("nil keys")
	}
	if err := id.Keys.Validate(); err != nil {
		t.Fatalf("invalid keys: %v", err)
	}

	// Verify encrypted file exists.
	path := filepath.Join(dir, identityFile)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("identity file not found: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("identity file is empty")
	}
}

func TestFingerprintStability(t *testing.T) {
	dir := t.TempDir()

	id1, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("first LoadOrCreate: %v", err)
	}

	id2, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("second LoadOrCreate: %v", err)
	}

	if id1.Fingerprint != id2.Fingerprint {
		t.Fatalf("fingerprint changed across loads: %s vs %s", id1.Fingerprint, id2.Fingerprint)
	}
}

func TestLoadNonexistent(t *testing.T) {
	dir := t.TempDir()

	_, err := Load(dir)
	if !os.IsNotExist(err) {
		t.Fatalf("expected os.IsNotExist, got: %v", err)
	}
}

func TestSaveAndReload(t *testing.T) {
	dir := t.TempDir()

	id, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	// Load explicitly.
	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if loaded.Fingerprint != id.Fingerprint {
		t.Fatalf("fingerprint mismatch: %s vs %s", loaded.Fingerprint, id.Fingerprint)
	}

	// Verify key round-trip: the loaded keys produce the same fingerprint.
	fp, err := loaded.Keys.Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint from loaded keys: %v", err)
	}
	if fp != id.Fingerprint {
		t.Fatalf("computed fingerprint mismatch: %s vs %s", fp, id.Fingerprint)
	}
}

func TestEncryptedFileNotPlaintext(t *testing.T) {
	dir := t.TempDir()

	_, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, identityFile))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}

	// Encrypted data should not contain JSON markers.
	for _, marker := range []string{`"x25519_seed"`, `"mlkem_seed"`, `"created_at"`} {
		if contains(data, []byte(marker)) {
			t.Fatalf("identity file contains plaintext marker %q", marker)
		}
	}
}

func contains(data, sub []byte) bool {
	for i := 0; i <= len(data)-len(sub); i++ {
		match := true
		for j := range sub {
			if data[i+j] != sub[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func TestKeyFunctionality(t *testing.T) {
	dir := t.TempDir()

	id, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	// Verify ML-KEM encapsulate/decapsulate round-trip.
	ss, ct := id.Keys.MlKemPublic.Encapsulate()
	ss2, err := id.Keys.MlKemPrivate.Decapsulate(ct)
	if err != nil {
		t.Fatalf("mlkem decapsulate: %v", err)
	}
	if len(ss) != len(ss2) {
		t.Fatal("shared secret length mismatch")
	}
	for i := range ss {
		if ss[i] != ss2[i] {
			t.Fatal("shared secret mismatch")
		}
	}

	// Verify X25519 ECDH.
	_, err = id.Keys.X25519Private.ECDH(id.Keys.X25519Public)
	if err != nil {
		t.Fatalf("x25519 ECDH: %v", err)
	}
}
