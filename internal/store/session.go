package store

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/chacha20poly1305"
)

// A session is libgm.AuthData marshalled to JSON. There is one per logged-in
// account, stored outside the SQLite database at
// $AGENT_GM_DATA_DIR/sessions/<acct_id>.enc, encrypted with the single data
// key using XChaCha20-Poly1305 with a random 24-byte nonce prefix
// (spec section 3.3):
//
//	sessions/<acct_id>.enc :=
//	    "AGMS1" || nonce[24] ||
//	    AEAD(datakey, nonce, json(AuthData), aad="agent-gm/session/v1|" || acct_id)
//
// The account ID is in the AEAD's associated data, so a session file renamed
// or copied to another account's name fails to open rather than silently
// loading the wrong account.

const sessionMagic = "AGMS1"

// ErrSessionUndecryptable means the data key differs from the one that sealed
// the session. There is no in-place rotation; restore the original key.
var ErrSessionUndecryptable = errors.New("session envelope cannot be decrypted")

// ErrNoSession means this account has no session file on disk.
var ErrNoSession = errors.New("no session file for this account")

// SessionStore reads and writes the per-account session files.
type SessionStore struct {
	dir string
	key DataKey
}

// NewSessionStore creates sessions/ at mode 0700 under the data directory.
func NewSessionStore(dataDir string, key DataKey) (*SessionStore, error) {
	dir := filepath.Join(dataDir, "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating %s: %w", dir, err)
	}
	// A directory restored from a backup may carry looser modes.
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("securing %s: %w", dir, err)
	}
	return &SessionStore{dir: dir, key: key}, nil
}

// Dir is the sessions directory.
func (s *SessionStore) Dir() string { return s.dir }

// Path is where one account's session lives.
func (s *SessionStore) Path(accountID string) string {
	return filepath.Join(s.dir, accountID+".enc")
}

func (s *SessionStore) aead() ([]byte, error) { return s.key.Derive(InfoSession) }

func aad(accountID string) []byte { return []byte(InfoSession + "|" + accountID) }

// Save writes the session atomically: a temp file in the same directory, an
// fsync, then a rename. The temp name is per account, so two accounts
// persisting at once cannot collide. Mode is 0600.
func (s *SessionStore) Save(accountID string, plaintext []byte) error {
	sub, err := s.aead()
	if err != nil {
		return err
	}
	c, err := chacha20poly1305.NewX(sub)
	if err != nil {
		return err
	}
	nonce := make([]byte, chacha20poly1305.NonceSizeX)
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("generating session nonce: %w", err)
	}
	body := c.Seal(nil, nonce, plaintext, aad(accountID))

	out := make([]byte, 0, len(sessionMagic)+len(nonce)+len(body))
	out = append(out, sessionMagic...)
	out = append(out, nonce...)
	out = append(out, body...)

	target := s.Path(accountID)
	tmp := target + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("opening %s: %w", tmp, err)
	}
	if _, err := f.Write(out); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("syncing %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("closing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("renaming %s: %w", tmp, err)
	}
	return nil
}

// Load reads and decrypts one account's session.
func (s *SessionStore) Load(accountID string) ([]byte, error) {
	data, err := os.ReadFile(s.Path(accountID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoSession
		}
		return nil, err
	}
	if len(data) < len(sessionMagic)+chacha20poly1305.NonceSizeX {
		return nil, ErrSessionUndecryptable
	}
	if string(data[:len(sessionMagic)]) != sessionMagic {
		return nil, ErrSessionUndecryptable
	}
	nonce := data[len(sessionMagic) : len(sessionMagic)+chacha20poly1305.NonceSizeX]
	body := data[len(sessionMagic)+chacha20poly1305.NonceSizeX:]

	sub, err := s.aead()
	if err != nil {
		return nil, err
	}
	c, err := chacha20poly1305.NewX(sub)
	if err != nil {
		return nil, err
	}
	plain, err := c.Open(nil, nonce, body, aad(accountID))
	if err != nil {
		return nil, ErrSessionUndecryptable
	}
	return plain, nil
}

// Present reports whether an account has a session file on disk.
func (s *SessionStore) Present(accountID string) bool {
	_, err := os.Stat(s.Path(accountID))
	return err == nil
}

// Shred removes one account's session file. Signing out shreds the file and
// zeroes the in-memory AuthData; it deletes no rows (spec section 4.7).
func (s *SessionStore) Shred(accountID string) error {
	err := os.Remove(s.Path(accountID))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
