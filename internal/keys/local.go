package keys

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// EnvKeyFile overrides the default keyfile location (ACR-700 §3.1, Q1). When
// set, its value is an absolute path to the 32-byte master key.
const EnvKeyFile = "ACP_KEY_FILE"

// defaultKeyFileSubpath is appended to the operator's home directory when
// $ACP_KEY_FILE is unset.
const defaultKeyFileSubpath = ".acp/master.key"

// LocalKeyfileProvider is the reference KeyProvider implementation: a single
// 32-byte file on disk, owner-readable only, generated on first start if
// absent. It satisfies ACR-700 §§3.1–3.2 without rotation; rotation support
// (Part 3.3) arrives in a later chunk.
type LocalKeyfileProvider struct {
	path string

	mu      sync.RWMutex
	key     [KeySize]byte
	version uint32
	loaded  bool

	// firstStart is set once, at construction time, if this process generated
	// the keyfile. Callers read it via WasFirstStart to decide whether to emit
	// the one-shot warning / audit entry.
	firstStart bool
}

// DefaultKeyfilePath resolves the on-disk keyfile path following ACR-700 §3.1:
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

// NewLocalKeyfileProvider loads or generates the master key at path. When path
// is empty, DefaultKeyfilePath is used.
//
// Loading an existing file runs the pre-flight permission check (§3.1):
// mode 0600, owner == process UID. Failing the check aborts construction
// before the provider accepts requests.
//
// Generating a fresh file writes the key with mode 0600 (creating parent dirs
// with mode 0700) and records the fact so callers can emit the one-shot
// warning through WasFirstStart.
func NewLocalKeyfileProvider(path string) (*LocalKeyfileProvider, error) {
	if path == "" {
		resolved, err := DefaultKeyfilePath()
		if err != nil {
			return nil, err
		}
		path = resolved
	}

	p := &LocalKeyfileProvider{path: path, version: FirstVersion}

	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := p.generate(); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, fmt.Errorf("acp keys: stat keyfile %q: %w", path, err)
	default:
		if err := checkPerms(path, info); err != nil {
			return nil, err
		}
		if err := p.load(); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// Path returns the keyfile path this provider was constructed against.
func (p *LocalKeyfileProvider) Path() string { return p.path }

// Fingerprint is the 16-hex-char SHA-256 truncation of the current key; used
// by startup messaging (ACR-700 §3.2).
func (p *LocalKeyfileProvider) Fingerprint() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return Fingerprint(p.key)
}

// WasFirstStart reports whether this process generated the keyfile during
// construction. Callers use it to fire the one-shot warning exactly once per
// fresh generation.
func (p *LocalKeyfileProvider) WasFirstStart() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.firstStart
}

// Current returns the active master key and its version.
func (p *LocalKeyfileProvider) Current() ([KeySize]byte, uint32, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.loaded {
		return [KeySize]byte{}, 0, errors.New("acp keys: provider not initialized")
	}
	return p.key, p.version, nil
}

// At looks up a historical key version. Until rotation lands, only
// FirstVersion is resolvable; every other version yields ErrKeyVersionUnavailable.
func (p *LocalKeyfileProvider) At(version uint32) ([KeySize]byte, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.loaded {
		return [KeySize]byte{}, errors.New("acp keys: provider not initialized")
	}
	if version != p.version {
		return [KeySize]byte{}, ErrKeyVersionUnavailable
	}
	return p.key, nil
}

func (p *LocalKeyfileProvider) load() error {
	f, err := os.Open(p.path)
	if err != nil {
		return fmt.Errorf("acp keys: open keyfile %q: %w", p.path, err)
	}
	defer f.Close()

	var buf [KeySize]byte
	n, err := io.ReadFull(f, buf[:])
	if err != nil {
		return fmt.Errorf("acp keys: read keyfile %q: %w", p.path, err)
	}
	if n != KeySize {
		return fmt.Errorf("acp keys: keyfile %q has %d bytes, want %d", p.path, n, KeySize)
	}
	// Reject trailing data so operators cannot smuggle extra bytes into a
	// file that silently truncates to the first 32.
	var trailing [1]byte
	if _, err := f.Read(trailing[:]); err == nil {
		return fmt.Errorf("acp keys: keyfile %q has more than %d bytes", p.path, KeySize)
	}

	p.mu.Lock()
	p.key = buf
	p.loaded = true
	p.mu.Unlock()
	return nil
}

func (p *LocalKeyfileProvider) generate() error {
	dir := filepath.Dir(p.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("acp keys: create keyfile dir %q: %w", dir, err)
	}
	// Harden in case MkdirAll is a no-op on an existing laxer directory.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("acp keys: chmod keyfile dir %q: %w", dir, err)
	}

	var buf [KeySize]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Errorf("acp keys: generate random key: %w", err)
	}

	// O_EXCL so a concurrent start cannot race us into overwriting a key that
	// another process just generated.
	f, err := os.OpenFile(p.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("acp keys: create keyfile %q: %w", p.path, err)
	}
	if _, err := f.Write(buf[:]); err != nil {
		_ = f.Close()
		return fmt.Errorf("acp keys: write keyfile %q: %w", p.path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("acp keys: close keyfile %q: %w", p.path, err)
	}

	p.mu.Lock()
	p.key = buf
	p.loaded = true
	p.firstStart = true
	p.mu.Unlock()
	return nil
}

// checkPerms enforces ACR-700 §3.1: mode 0600, owner == process UID.
// Non-owner readability or wrong ownership aborts before we ever accept
// traffic.
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
		// Non-Unix filesystems (rare on servers) cannot enforce ownership;
		// refuse rather than silently allowing an escalation vector.
		return fmt.Errorf("acp keys: keyfile %q has no ownership info (unsupported platform)", path)
	}
	if uid := uint32(os.Getuid()); stat.Uid != uid {
		return fmt.Errorf("acp keys: keyfile %q is owned by uid %d, process uid is %d", path, stat.Uid, uid)
	}
	return nil
}
