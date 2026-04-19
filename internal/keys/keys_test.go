package keys

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFingerprintDeterministic(t *testing.T) {
	var k [KeySize]byte
	for i := range k {
		k[i] = byte(i)
	}
	got := Fingerprint(k)
	if len(got) != 16 {
		t.Fatalf("fingerprint length = %d, want 16", len(got))
	}
	if got != Fingerprint(k) {
		t.Fatal("fingerprint is not deterministic")
	}
}

func TestNewLocalKeyfileProviderFirstStart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "master.key")

	p, err := NewLocalKeyfileProvider(path)
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	if !p.WasFirstStart() {
		t.Error("WasFirstStart = false, want true after fresh generation")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat keyfile: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("keyfile mode = %#o, want 0600", mode)
	}
	if info.Size() != KeySize {
		t.Errorf("keyfile size = %d, want %d", info.Size(), KeySize)
	}

	parent, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat parent dir: %v", err)
	}
	if mode := parent.Mode().Perm(); mode != 0o700 {
		t.Errorf("parent dir mode = %#o, want 0700", mode)
	}

	key, version, err := p.Current()
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if version != FirstVersion {
		t.Errorf("version = %d, want %d", version, FirstVersion)
	}
	if key == ([KeySize]byte{}) {
		t.Error("generated key is all zeros")
	}
	if p.Fingerprint() != Fingerprint(key) {
		t.Error("provider fingerprint does not match Current key fingerprint")
	}
}

func TestNewLocalKeyfileProviderLoadsExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")

	want := bytes.Repeat([]byte{0xAB}, KeySize)
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatalf("seed keyfile: %v", err)
	}

	p, err := NewLocalKeyfileProvider(path)
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	if p.WasFirstStart() {
		t.Error("WasFirstStart = true, want false when loading existing key")
	}
	key, _, err := p.Current()
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if !bytes.Equal(key[:], want) {
		t.Error("loaded key does not match on-disk bytes")
	}
}

func TestNewLocalKeyfileProviderRejectsLoosePerms(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	if err := os.WriteFile(path, bytes.Repeat([]byte{0x01}, KeySize), 0o644); err != nil {
		t.Fatalf("seed keyfile: %v", err)
	}

	if _, err := NewLocalKeyfileProvider(path); err == nil {
		t.Fatal("expected error for 0644 keyfile, got nil")
	}
}

func TestNewLocalKeyfileProviderRejectsWrongSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	if err := os.WriteFile(path, []byte("too short"), 0o600); err != nil {
		t.Fatalf("seed keyfile: %v", err)
	}
	if _, err := NewLocalKeyfileProvider(path); err == nil {
		t.Fatal("expected error for undersized keyfile, got nil")
	}

	if err := os.WriteFile(path, bytes.Repeat([]byte{0x02}, KeySize+1), 0o600); err != nil {
		t.Fatalf("seed keyfile: %v", err)
	}
	if _, err := NewLocalKeyfileProvider(path); err == nil {
		t.Fatal("expected error for oversized keyfile, got nil")
	}
}

func TestAtRejectsUnknownVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")

	p, err := NewLocalKeyfileProvider(path)
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}

	if _, err := p.At(FirstVersion); err != nil {
		t.Errorf("At(FirstVersion) unexpected error: %v", err)
	}
	if _, err := p.At(FirstVersion + 1); !errors.Is(err, ErrKeyVersionUnavailable) {
		t.Errorf("At(next) = %v, want ErrKeyVersionUnavailable", err)
	}
	if _, err := p.At(0); !errors.Is(err, ErrKeyVersionUnavailable) {
		t.Errorf("At(0) = %v, want ErrKeyVersionUnavailable", err)
	}
}

func TestDefaultKeyfilePathRespectsEnv(t *testing.T) {
	t.Setenv(EnvKeyFile, "/absolute/path/master.key")
	got, err := DefaultKeyfilePath()
	if err != nil {
		t.Fatalf("DefaultKeyfilePath: %v", err)
	}
	if got != "/absolute/path/master.key" {
		t.Errorf("path = %q, want /absolute/path/master.key", got)
	}
}

func TestDefaultKeyfilePathRejectsRelativeEnv(t *testing.T) {
	t.Setenv(EnvKeyFile, "relative/master.key")
	if _, err := DefaultKeyfilePath(); err == nil {
		t.Fatal("expected error for relative $ACP_KEY_FILE, got nil")
	}
}

func TestDefaultKeyfilePathFallsBackToHome(t *testing.T) {
	t.Setenv(EnvKeyFile, "")
	t.Setenv("HOME", "/tmp/acp-test-home")
	got, err := DefaultKeyfilePath()
	if err != nil {
		t.Fatalf("DefaultKeyfilePath: %v", err)
	}
	want := filepath.Join("/tmp/acp-test-home", defaultKeyFileSubpath)
	if got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
}
