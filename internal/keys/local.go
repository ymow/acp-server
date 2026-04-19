package keys

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"syscall"
)

// EnvKeyFile overrides the default keyfile location (ACR-700 §3.1, Q1). When
// set, its value is an absolute path to the 32-byte master key (legacy
// single-file layout). The containing directory anchors the keyring.
const EnvKeyFile = "ACP_KEY_FILE"

// defaultKeyFileSubpath is appended to the operator's home directory when
// $ACP_KEY_FILE is unset. Preserved for backwards compatibility: the
// containing directory is where the keyring subdir lives.
const defaultKeyFileSubpath = ".acp/master.key"

// keyringSubdir is the directory under the keyring anchor that holds the
// per-version key files (ACR-700 §3.3). Keeping it a sibling of the legacy
// master.key file means existing deployments don't need to move $ACP_KEY_FILE.
const keyringSubdir = "keys"

// versionedKeyPattern matches keyring files of the form v{N}.key with N >= 1.
// Using a strict regex stops the provider from accidentally picking up
// editor backups, swap files, or archived ".bak" copies.
var versionedKeyPattern = regexp.MustCompile(`^v([1-9][0-9]*)\.key$`)

// LocalKeyfileProvider is the reference KeyProvider: a directory of
// per-version 32-byte files on disk, owner-readable only, auto-generated on
// first start. Rotation (ACR-700 §3.3) bumps the current pointer to a newly
// created v{N+1}.key without touching earlier versions so historical
// ciphertext remains readable. The legacy single-file layout (4.5.1–4.5.4)
// is auto-migrated to v1.key on first construction.
type LocalKeyfileProvider struct {
	// legacyPath is where the pre-4.5.8 single master.key used to live. When
	// a keyring subdir does not yet exist, a file at this path is rotated
	// into the keyring as v1.key so upgraded deployments do not need manual
	// steps. After migration it is unlinked.
	legacyPath string
	// keyringDir is the directory that actually holds v{N}.key files. For
	// the default layout this is $HOME/.acp/keys.
	keyringDir string

	mu      sync.RWMutex
	keys    map[uint32][KeySize]byte
	current uint32

	// firstStart is set once, at construction time, if this process generated
	// the first v1.key (fresh install, no legacy file). Operators get a
	// one-shot warning so they remember to back it up.
	firstStart bool
	// migrated is set if this process rotated a legacy master.key into
	// keys/v1.key. Distinct from firstStart: the key material is not new,
	// only the on-disk location is.
	migrated bool
}

// DefaultKeyfilePath resolves the on-disk keyfile anchor following ACR-700 §3.1:
// $ACP_KEY_FILE wins if set, otherwise $HOME/.acp/master.key.
func DefaultKeyfilePath() (string, error) {
	if override := os.Getenv(EnvKeyFile); override != "" {
		if !filepath.IsAbs(override) {
			return "", fmt.Errorf("acp keys: %s must be an absolute path, got %q", EnvKeyFile, override)
		}
		return override, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("acp keys: resolve home dir: %w", err)
	}
	return filepath.Join(home, defaultKeyFileSubpath), nil
}

// NewLocalKeyfileProvider loads or generates the keyring anchored at path.
// When path is empty, DefaultKeyfilePath is used.
//
// Construction rules (evaluated in order):
//  1. If a keyring subdir exists with ≥1 versioned file, load every version
//     and set current = max(version).
//  2. Else if a legacy single-file key sits at path, rotate it into
//     keyringDir/v1.key and unlink the original.
//  3. Else first-start: generate keyringDir/v1.key with O_EXCL + 0600 and
//     set firstStart = true.
//
// Every loaded file runs the §3.1 permission check (mode 0600, owner == uid).
func NewLocalKeyfileProvider(path string) (*LocalKeyfileProvider, error) {
	if path == "" {
		resolved, err := DefaultKeyfilePath()
		if err != nil {
			return nil, err
		}
		path = resolved
	}

	p := &LocalKeyfileProvider{
		legacyPath: path,
		keyringDir: filepath.Join(filepath.Dir(path), keyringSubdir),
		keys:       map[uint32][KeySize]byte{},
	}

	entries, loaded, err := p.scanKeyring()
	if err != nil {
		return nil, err
	}

	switch {
	case loaded:
		if err := p.loadVersions(entries); err != nil {
			return nil, err
		}
	case legacyExists(p.legacyPath):
		if err := p.migrateLegacy(); err != nil {
			return nil, err
		}
	default:
		if err := p.generateFirstVersion(); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// Path returns the legacy keyfile anchor this provider was constructed against.
// The actual on-disk storage lives under KeyringDir.
func (p *LocalKeyfileProvider) Path() string { return p.legacyPath }

// KeyringDir returns the directory holding v{N}.key files.
func (p *LocalKeyfileProvider) KeyringDir() string { return p.keyringDir }

// Fingerprint is the 16-hex-char SHA-256 truncation of the current key; used
// by startup messaging (ACR-700 §3.2).
func (p *LocalKeyfileProvider) Fingerprint() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	k := p.keys[p.current]
	return Fingerprint(k)
}

// WasFirstStart reports whether this process generated the first v1.key
// during construction. Distinct from WasMigrated.
func (p *LocalKeyfileProvider) WasFirstStart() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.firstStart
}

// WasMigrated reports whether this process rotated a legacy master.key into
// the keyring during construction. Operators may want to emit a one-shot info
// log so they know the old file is gone on purpose.
func (p *LocalKeyfileProvider) WasMigrated() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.migrated
}

// Current returns the active master key and its version.
func (p *LocalKeyfileProvider) Current() ([KeySize]byte, uint32, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.current == 0 {
		return [KeySize]byte{}, 0, errors.New("acp keys: provider not initialized")
	}
	return p.keys[p.current], p.current, nil
}

// At looks up a historical key version. Any version the keyring does not
// contain yields ErrKeyVersionUnavailable so decrypt paths can report it
// without falling back.
func (p *LocalKeyfileProvider) At(version uint32) ([KeySize]byte, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.current == 0 {
		return [KeySize]byte{}, errors.New("acp keys: provider not initialized")
	}
	k, ok := p.keys[version]
	if !ok {
		return [KeySize]byte{}, ErrKeyVersionUnavailable
	}
	return k, nil
}

// Versions returns every key version the provider has loaded, sorted ascending.
// Exposed for diagnostics and the reencrypt tool (both need to know which
// versions exist without calling At for every possible number).
func (p *LocalKeyfileProvider) Versions() []uint32 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]uint32, 0, len(p.keys))
	for v := range p.keys {
		out = append(out, v)
	}
	// Simple insertion sort — versions counts are tiny.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// Rotate generates keys/v{current+1}.key with fresh random bytes, loads it,
// and bumps the current pointer. Existing ciphertext stays readable because
// the older versions remain on disk and in memory. Returns the new version
// and its fingerprint for operator-facing logging.
//
// Rotate does not re-encrypt existing rows; that is the reencrypt tool's job.
// Splitting the two keeps a rotation from blocking on a long scan.
func (p *LocalKeyfileProvider) Rotate() (uint32, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.current == 0 {
		return 0, "", errors.New("acp keys: provider not initialized")
	}
	nextVersion := p.current + 1
	if nextVersion == 0 {
		// Wrap-around in the unsigned counter. With u24 in the §2.3 header
		// this would need 16M rotations before it matters, but refuse
		// explicitly so the guard is visible.
		return 0, "", errors.New("acp keys: key_version overflow")
	}

	var buf [KeySize]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return 0, "", fmt.Errorf("acp keys: generate random key: %w", err)
	}

	filename := versionFilename(nextVersion)
	target := filepath.Join(p.keyringDir, filename)

	// O_EXCL so two concurrent `rotate-key` invocations cannot both claim the
	// next version — whichever process loses the race reports an error and
	// the operator retries with a fresh view of the keyring.
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, "", fmt.Errorf("acp keys: create %q: %w", target, err)
	}
	if _, err := f.Write(buf[:]); err != nil {
		_ = f.Close()
		_ = os.Remove(target)
		return 0, "", fmt.Errorf("acp keys: write %q: %w", target, err)
	}
	if err := f.Close(); err != nil {
		return 0, "", fmt.Errorf("acp keys: close %q: %w", target, err)
	}

	p.keys[nextVersion] = buf
	p.current = nextVersion
	return nextVersion, Fingerprint(buf), nil
}

// scanKeyring lists versioned key files in keyringDir. Returns (entries,
// true, nil) when the directory exists and holds ≥1 valid file; (nil, false,
// nil) when the directory does not exist; non-nil err on permission issues
// or a parse failure.
func (p *LocalKeyfileProvider) scanKeyring() (map[uint32]string, bool, error) {
	dirInfo, err := os.Stat(p.keyringDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("acp keys: stat keyring %q: %w", p.keyringDir, err)
	}
	if !dirInfo.IsDir() {
		return nil, false, fmt.Errorf("acp keys: keyring path %q is not a directory", p.keyringDir)
	}
	if err := checkDirPerms(p.keyringDir, dirInfo); err != nil {
		return nil, false, err
	}

	dirEntries, err := os.ReadDir(p.keyringDir)
	if err != nil {
		return nil, false, fmt.Errorf("acp keys: read keyring %q: %w", p.keyringDir, err)
	}
	out := map[uint32]string{}
	for _, entry := range dirEntries {
		if entry.IsDir() {
			continue
		}
		m := versionedKeyPattern.FindStringSubmatch(entry.Name())
		if m == nil {
			// Skip unrelated files — operators may put a README or .gitignore
			// in the dir. A strict reject here would make the keyring brittle.
			continue
		}
		v, err := strconv.ParseUint(m[1], 10, 32)
		if err != nil || v == 0 {
			return nil, false, fmt.Errorf("acp keys: keyring entry %q has invalid version", entry.Name())
		}
		out[uint32(v)] = filepath.Join(p.keyringDir, entry.Name())
	}
	return out, len(out) > 0, nil
}

// loadVersions reads every file in entries under permission checks, stores
// the key bytes, and sets p.current = max(version).
func (p *LocalKeyfileProvider) loadVersions(entries map[uint32]string) error {
	var max uint32
	for version, path := range entries {
		key, err := readKeyfile(path)
		if err != nil {
			return err
		}
		p.keys[version] = key
		if version > max {
			max = version
		}
	}
	p.current = max
	return nil
}

// migrateLegacy rotates a pre-4.5.8 master.key into keys/v1.key atomically
// (os.Rename on the same filesystem). After migration the legacy file is
// gone, which is deliberate — leaving it behind would confuse every future
// startup into re-running this code path.
func (p *LocalKeyfileProvider) migrateLegacy() error {
	info, err := os.Stat(p.legacyPath)
	if err != nil {
		return fmt.Errorf("acp keys: stat legacy keyfile %q: %w", p.legacyPath, err)
	}
	if err := checkPerms(p.legacyPath, info); err != nil {
		return err
	}
	if err := ensureDir(p.keyringDir); err != nil {
		return err
	}
	target := filepath.Join(p.keyringDir, versionFilename(FirstVersion))
	// The target must not already exist — scanKeyring returned no entries,
	// but guard against a TOCTOU race with a parallel start.
	if _, err := os.Stat(target); err == nil {
		return fmt.Errorf("acp keys: migration target %q already exists", target)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("acp keys: stat %q: %w", target, err)
	}
	if err := os.Rename(p.legacyPath, target); err != nil {
		return fmt.Errorf("acp keys: migrate %q → %q: %w", p.legacyPath, target, err)
	}

	key, err := readKeyfile(target)
	if err != nil {
		return err
	}
	p.keys[FirstVersion] = key
	p.current = FirstVersion
	p.migrated = true
	return nil
}

// generateFirstVersion creates the keyring dir and writes v1.key with fresh
// random bytes. This is the first-install code path.
func (p *LocalKeyfileProvider) generateFirstVersion() error {
	if err := ensureDir(p.keyringDir); err != nil {
		return err
	}
	var buf [KeySize]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Errorf("acp keys: generate random key: %w", err)
	}
	target := filepath.Join(p.keyringDir, versionFilename(FirstVersion))
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("acp keys: create %q: %w", target, err)
	}
	if _, err := f.Write(buf[:]); err != nil {
		_ = f.Close()
		return fmt.Errorf("acp keys: write %q: %w", target, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("acp keys: close %q: %w", target, err)
	}

	p.keys[FirstVersion] = buf
	p.current = FirstVersion
	p.firstStart = true
	return nil
}

// versionFilename formats a keyring filename for the given version.
func versionFilename(v uint32) string {
	return fmt.Sprintf("v%d.key", v)
}

// legacyExists reports whether a readable file sits at the legacy anchor. A
// stat error that is not "not exist" surfaces as "does not exist" here and
// will re-surface in migrateLegacy with a clearer error — avoids double-reporting.
func legacyExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

// ensureDir makes keyringDir (and intermediate dirs) with mode 0700, then
// hardens the mode in case MkdirAll was a no-op on an existing laxer dir.
func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("acp keys: create keyring dir %q: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("acp keys: chmod keyring dir %q: %w", dir, err)
	}
	return nil
}

// readKeyfile enforces ACR-700 §3.1 and returns the 32-byte payload.
func readKeyfile(path string) ([KeySize]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return [KeySize]byte{}, fmt.Errorf("acp keys: stat %q: %w", path, err)
	}
	if err := checkPerms(path, info); err != nil {
		return [KeySize]byte{}, err
	}

	f, err := os.Open(path)
	if err != nil {
		return [KeySize]byte{}, fmt.Errorf("acp keys: open %q: %w", path, err)
	}
	defer f.Close()

	var buf [KeySize]byte
	n, err := io.ReadFull(f, buf[:])
	if err != nil {
		return [KeySize]byte{}, fmt.Errorf("acp keys: read %q: %w", path, err)
	}
	if n != KeySize {
		return [KeySize]byte{}, fmt.Errorf("acp keys: %q has %d bytes, want %d", path, n, KeySize)
	}
	var trailing [1]byte
	if _, err := f.Read(trailing[:]); err == nil {
		return [KeySize]byte{}, fmt.Errorf("acp keys: %q has more than %d bytes", path, KeySize)
	}
	return buf, nil
}

// checkPerms enforces ACR-700 §3.1: mode 0600, owner == process UID.
func checkPerms(path string, info fs.FileInfo) error {
	if info.IsDir() {
		return fmt.Errorf("acp keys: keyfile %q is a directory", path)
	}
	mode := info.Mode().Perm()
	if mode != 0o600 {
		return fmt.Errorf("acp keys: keyfile %q has mode %#o, want 0600", path, mode)
	}
	if info.Size() != KeySize {
		return fmt.Errorf("acp keys: keyfile %q is %d bytes, want %d", path, info.Size(), KeySize)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("acp keys: keyfile %q has no ownership info (unsupported platform)", path)
	}
	if uid := uint32(os.Getuid()); stat.Uid != uid {
		return fmt.Errorf("acp keys: keyfile %q is owned by uid %d, process uid is %d", path, stat.Uid, uid)
	}
	return nil
}

// checkDirPerms enforces ACR-700 §3.1 at directory granularity: mode 0700,
// owner == process UID. World- or group-traversable keyring dirs are a risk
// even before any file is read.
func checkDirPerms(path string, info fs.FileInfo) error {
	mode := info.Mode().Perm()
	if mode != 0o700 {
		return fmt.Errorf("acp keys: keyring dir %q has mode %#o, want 0700", path, mode)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("acp keys: keyring dir %q has no ownership info (unsupported platform)", path)
	}
	if uid := uint32(os.Getuid()); stat.Uid != uid {
		return fmt.Errorf("acp keys: keyring dir %q is owned by uid %d, process uid is %d", path, stat.Uid, uid)
	}
	return nil
}
