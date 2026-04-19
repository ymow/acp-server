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
	if p.WasMigrated() {
		t.Error("WasMigrated = true on fresh install")
	}

	// 4.5.8: the provider writes keys/v1.key, not the legacy anchor path.
	v1Path := filepath.Join(filepath.Dir(path), keyringSubdir, "v1.key")
	info, err := os.Stat(v1Path)
	if err != nil {
		t.Fatalf("stat v1.key: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("v1.key mode = %#o, want 0600", mode)
	}
	if info.Size() != KeySize {
		t.Errorf("v1.key size = %d, want %d", info.Size(), KeySize)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("legacy anchor %q should not exist after first start, got err=%v", path, err)
	}

	keyringDirInfo, err := os.Stat(filepath.Join(filepath.Dir(path), keyringSubdir))
	if err != nil {
		t.Fatalf("stat keyring dir: %v", err)
	}
	if mode := keyringDirInfo.Mode().Perm(); mode != 0o700 {
		t.Errorf("keyring dir mode = %#o, want 0700", mode)
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
	// Loading a pre-populated keyring (post-4.5.8 layout) must not look like
	// a fresh install nor a legacy migration.
	dir := t.TempDir()
	keyringDir := filepath.Join(dir, keyringSubdir)
	if err := os.Mkdir(keyringDir, 0o700); err != nil {
		t.Fatalf("mkdir keyring: %v", err)
	}
	want := bytes.Repeat([]byte{0xAB}, KeySize)
	if err := os.WriteFile(filepath.Join(keyringDir, "v1.key"), want, 0o600); err != nil {
		t.Fatalf("seed v1.key: %v", err)
	}

	p, err := NewLocalKeyfileProvider(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	if p.WasFirstStart() {
		t.Error("WasFirstStart = true, want false when loading existing keyring")
	}
	if p.WasMigrated() {
		t.Error("WasMigrated = true when keyring already existed")
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
	keyringDir := filepath.Join(dir, keyringSubdir)
	if err := os.Mkdir(keyringDir, 0o700); err != nil {
		t.Fatalf("mkdir keyring: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(keyringDir, "v1.key"),
		bytes.Repeat([]byte{0x01}, KeySize),
		0o644,
	); err != nil {
		t.Fatalf("seed v1.key: %v", err)
	}

	if _, err := NewLocalKeyfileProvider(filepath.Join(dir, "master.key")); err == nil {
		t.Fatal("expected error for 0644 keyfile, got nil")
	}
}

func TestNewLocalKeyfileProviderRejectsWrongSize(t *testing.T) {
	dir := t.TempDir()
	keyringDir := filepath.Join(dir, keyringSubdir)
	if err := os.Mkdir(keyringDir, 0o700); err != nil {
		t.Fatalf("mkdir keyring: %v", err)
	}
	v1 := filepath.Join(keyringDir, "v1.key")

	if err := os.WriteFile(v1, []byte("too short"), 0o600); err != nil {
		t.Fatalf("seed v1.key: %v", err)
	}
	if _, err := NewLocalKeyfileProvider(filepath.Join(dir, "master.key")); err == nil {
		t.Fatal("expected error for undersized keyfile, got nil")
	}

	if err := os.WriteFile(v1, bytes.Repeat([]byte{0x02}, KeySize+1), 0o600); err != nil {
		t.Fatalf("seed v1.key: %v", err)
	}
	if _, err := NewLocalKeyfileProvider(filepath.Join(dir, "master.key")); err == nil {
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

// Legacy migration: a pre-4.5.8 deployment has ~/.acp/master.key but no
// ~/.acp/keys/ subdir. First construction after the upgrade must rotate the
// file into the keyring and remove the original so subsequent starts take
// the normal load path.
func TestNewLocalKeyfileProviderMigratesLegacyFile(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "master.key")

	want := bytes.Repeat([]byte{0xCD}, KeySize)
	if err := os.WriteFile(legacy, want, 0o600); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}

	p, err := NewLocalKeyfileProvider(legacy)
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	if !p.WasMigrated() {
		t.Error("WasMigrated = false, want true after legacy migration")
	}
	if p.WasFirstStart() {
		t.Error("WasFirstStart = true on legacy migration (key material is not new)")
	}

	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("legacy file %q should be gone after migration, err=%v", legacy, err)
	}
	v1 := filepath.Join(dir, keyringSubdir, "v1.key")
	got, err := os.ReadFile(v1)
	if err != nil {
		t.Fatalf("read v1.key: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Error("migrated key does not match legacy bytes")
	}

	// Construct again — should now take the "load keyring" path with neither
	// flag set; key material identical.
	p2, err := NewLocalKeyfileProvider(legacy)
	if err != nil {
		t.Fatalf("reload provider: %v", err)
	}
	if p2.WasMigrated() || p2.WasFirstStart() {
		t.Error("second construction should be a plain load")
	}
	k, v, err := p2.Current()
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if v != FirstVersion {
		t.Errorf("version = %d, want %d", v, FirstVersion)
	}
	if !bytes.Equal(k[:], want) {
		t.Error("reload yielded different bytes than migration")
	}
}

func TestRotateAppendsVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	p, err := NewLocalKeyfileProvider(path)
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}

	origKey, origVer, _ := p.Current()
	if origVer != FirstVersion {
		t.Fatalf("setup: Current version = %d, want %d", origVer, FirstVersion)
	}

	nextVer, fp, err := p.Rotate()
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if nextVer != FirstVersion+1 {
		t.Errorf("rotate version = %d, want %d", nextVer, FirstVersion+1)
	}
	if len(fp) != 16 {
		t.Errorf("rotate fingerprint len = %d, want 16", len(fp))
	}

	// v2.key is on disk and permissioned correctly.
	v2 := filepath.Join(dir, keyringSubdir, "v2.key")
	info, err := os.Stat(v2)
	if err != nil {
		t.Fatalf("stat v2.key: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("v2.key mode = %#o, want 0600", mode)
	}

	// Current now points at v2 with a different key than v1 (random gen).
	nowKey, nowVer, err := p.Current()
	if err != nil {
		t.Fatalf("Current after rotate: %v", err)
	}
	if nowVer != FirstVersion+1 {
		t.Errorf("current version after rotate = %d, want %d", nowVer, FirstVersion+1)
	}
	if bytes.Equal(nowKey[:], origKey[:]) {
		t.Error("rotate produced same key bytes — rand gen not working")
	}

	// T4: ciphertext written under v1 must still decrypt after rotation.
	// We verify at the key layer — At(v1) returns the original bytes.
	v1Key, err := p.At(FirstVersion)
	if err != nil {
		t.Fatalf("At(v1) after rotate: %v", err)
	}
	if !bytes.Equal(v1Key[:], origKey[:]) {
		t.Error("At(v1) after rotate returned different bytes")
	}

	// Versions() exposes both.
	vs := p.Versions()
	if len(vs) != 2 || vs[0] != FirstVersion || vs[1] != FirstVersion+1 {
		t.Errorf("Versions() = %v, want [1 2]", vs)
	}
}

// Second Rotate must bump to v3, not overwrite v2. Regression guard for a
// bug where the in-memory current pointer advances but the on-disk filename
// was computed against the wrong version.
func TestRotateTwice(t *testing.T) {
	dir := t.TempDir()
	p, err := NewLocalKeyfileProvider(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	if _, _, err := p.Rotate(); err != nil {
		t.Fatalf("rotate 1: %v", err)
	}
	v3, _, err := p.Rotate()
	if err != nil {
		t.Fatalf("rotate 2: %v", err)
	}
	if v3 != FirstVersion+2 {
		t.Errorf("second rotate version = %d, want %d", v3, FirstVersion+2)
	}
	for _, v := range []uint32{1, 2, 3} {
		if _, err := os.Stat(filepath.Join(dir, keyringSubdir, versionFilename(v))); err != nil {
			t.Errorf("missing v%d.key: %v", v, err)
		}
	}
}

// If the keyring dir has loose perms (say, an operator copied it from
// elsewhere and it landed 0755), refuse to load rather than silently widening
// the trust boundary.
func TestNewLocalKeyfileProviderRejectsLooseKeyringDir(t *testing.T) {
	dir := t.TempDir()
	keyringDir := filepath.Join(dir, keyringSubdir)
	if err := os.Mkdir(keyringDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(keyringDir, "v1.key"),
		bytes.Repeat([]byte{0x11}, KeySize),
		0o600,
	); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := NewLocalKeyfileProvider(filepath.Join(dir, "master.key")); err == nil {
		t.Fatal("expected error for 0755 keyring dir, got nil")
	}
}
