package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/thisnick/agent-gm/internal/config"
)

// Credential file layout, spec section 11.5. Tokens are stored per server in
// $XDG_STATE_HOME/agent-gm/credentials.json, mode 0600 inside a 0700
// directory, written atomically under a credentials.lock advisory lock.
const (
	// CredentialsFileName is the file inside the state directory.
	CredentialsFileName = "credentials.json"
	// CredentialsLockName is the advisory lock beside it.
	CredentialsLockName = "credentials.lock"
	// credentialsFileMode is 0600: the file holds bearer tokens.
	credentialsFileMode os.FileMode = 0o600
	// credentialsDirMode is 0700.
	credentialsDirMode os.FileMode = 0o700
	// DefaultProfile is the profile name used when --profile is not given.
	DefaultProfile = "default"
)

// Profile is one server's stored authorization. It is bound to an exact
// server: changing --server selects the credentials for THAT server and
// never forwards one profile's token to a new origin (spec section 11.5).
type Profile struct {
	Server          string   `json:"server"`
	AccessToken     string   `json:"access_token,omitempty"`
	RefreshToken    string   `json:"refresh_token,omitempty"`
	Scopes          []string `json:"scopes,omitempty"`
	ExpiresAt       string   `json:"expires_at,omitempty"`
	AuthorizationID string   `json:"authorization_id,omitempty"`
	Issuer          string   `json:"issuer,omitempty"`
	Resource        string   `json:"resource,omitempty"`
}

// Credentials is the whole file.
type Credentials struct {
	Version  int                `json:"version"`
	Profiles map[string]Profile `json:"profiles"`
}

// CredentialsVersion is the on-disk format version.
const CredentialsVersion = 1

// Store is the credentials file and the directory it lives in.
type Store struct{ Path string }

// CredentialsPath resolves the file per spec section 11.5:
// --credentials-file, else AGENT_GM_CREDENTIALS_FILE, else
// $XDG_STATE_HOME/agent-gm/credentials.json.
func CredentialsPath(flagValue string, getenv func(string) string) string {
	if flagValue != "" {
		return flagValue
	}
	if v := getenv("AGENT_GM_CREDENTIALS_FILE"); v != "" {
		return v
	}
	return filepath.Join(config.StateDir(), CredentialsFileName)
}

// NewStore builds a store for a path.
func NewStore(path string) *Store { return &Store{Path: path} }

// Dir is the directory the file and its lock live in.
func (s *Store) Dir() string { return filepath.Dir(s.Path) }

// Load reads the file. A missing file is an empty set of profiles, not an
// error: the first `agm auth login` on a machine has nothing to read.
func (s *Store) Load() (Credentials, error) {
	c := Credentials{Version: CredentialsVersion, Profiles: map[string]Profile{}}
	raw, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, localErr(err, "the credentials file %s could not be read", s.Path)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return c, nil
	}
	var onDisk Credentials
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		return c, localErr(err, "the credentials file %s is not valid JSON", s.Path)
	}
	if onDisk.Profiles == nil {
		onDisk.Profiles = map[string]Profile{}
	}
	if onDisk.Version == 0 {
		onDisk.Version = CredentialsVersion
	}
	return onDisk, nil
}

// Begin proves the destination writable and takes the advisory lock, WITHOUT
// writing anything yet.
//
// This is spec section 11.5's write-back safety rule, and the ordering is the
// whole of it: the temporary file that the write will later rename into place
// is created FIRST, before the token is spent. An unwritable directory is
// therefore exit 9 while the token is still good -- nothing was consumed, so
// the operator fixes the machine and runs again. Rotation makes a refresh
// token single-use, so a run that exchanges one and then cannot store the
// replacement has lost it; that is the failure this ordering exists to
// prevent.
func (s *Store) Begin() (*PendingWrite, error) {
	return beginCredentialsWrite(s.Path, filepath.Join(s.Dir(), CredentialsLockName))
}

// PendingWrite is a proved-writable destination: a held lock and an open
// temporary file in the same directory as the destination, ready to be
// renamed into place.
type PendingWrite struct {
	final string
	tmp   *os.File
	lock  *os.File
	done  bool
}

// beginCredentialsWrite is beginWrite for the credentials file itself, which
// Agent GM owns: it creates the directory at 0700 and asserts that mode on an
// existing one, because section 11.5 states it.
//
// beginWrite does NOT do that, and the difference matters: the refresh token
// file is the operator's, at a path they chose, and a CLI that quietly
// widened or narrowed the mode of a directory it was merely writing into
// would be changing something it was not asked to change -- and would defeat
// the very check the write-back safety rule depends on.
func beginCredentialsWrite(final, lockPath string) (*PendingWrite, error) {
	dir := filepath.Dir(final)
	if err := os.MkdirAll(dir, credentialsDirMode); err != nil {
		return nil, localErr(err, "the credentials directory %s could not be created", dir)
	}
	if err := os.Chmod(dir, credentialsDirMode); err != nil {
		return nil, localErr(err, "the credentials directory %s could not be set to 0700", dir)
	}
	return beginWrite(final, lockPath)
}

// beginWrite takes the lock when one is named and creates the temporary file
// at 0600, proving the destination writable without writing to it. Every
// failure is a *LocalError, which is exit 9.
func beginWrite(final, lockPath string) (*PendingWrite, error) {
	dir := filepath.Dir(final)

	p := &PendingWrite{final: final}
	if lockPath != "" {
		lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, credentialsFileMode)
		if err != nil {
			return nil, localErr(err, "the credentials lock %s could not be opened", lockPath)
		}
		if err := lockFile(lock); err != nil {
			_ = lock.Close()
			return nil, localErr(err, "the credentials lock %s could not be taken", lockPath)
		}
		p.lock = lock
	}

	tmp, err := os.CreateTemp(dir, filepath.Base(final)+".tmp-*")
	if err != nil {
		p.release()
		return nil, localErr(err, "%s is not writable: the temporary file the write renames "+
			"into place could not be created. Nothing was spent", dir)
	}
	if err := tmp.Chmod(credentialsFileMode); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		p.release()
		return nil, localErr(err, "the temporary credentials file could not be set to 0600")
	}
	p.tmp = tmp
	return p, nil
}

// Commit writes the bytes and renames them into place atomically.
func (p *PendingWrite) Commit(data []byte) error {
	if p.done {
		return &LocalError{Msg: "the credentials write was already finished"}
	}
	defer func() { p.done = true }()

	name := p.tmp.Name()
	fail := func(err error, what string) error {
		_ = p.tmp.Close()
		_ = os.Remove(name)
		p.release()
		return localErr(err, "%s", what)
	}
	if _, err := p.tmp.Write(data); err != nil {
		return fail(err, "the credentials could not be written")
	}
	if err := p.tmp.Sync(); err != nil {
		return fail(err, "the credentials could not be flushed to disk")
	}
	if err := p.tmp.Close(); err != nil {
		return fail(err, "the temporary credentials file could not be closed")
	}
	if err := os.Rename(name, p.final); err != nil {
		_ = os.Remove(name)
		p.release()
		return localErr(err, "the credentials could not be renamed into place at %s", p.final)
	}
	p.release()
	return nil
}

// Close abandons the write, removing the temporary file and dropping the
// lock. It is safe to call after Commit.
func (p *PendingWrite) Close() {
	if p.done {
		return
	}
	p.done = true
	if p.tmp != nil {
		name := p.tmp.Name()
		_ = p.tmp.Close()
		_ = os.Remove(name)
	}
	p.release()
}

func (p *PendingWrite) release() {
	if p.lock != nil {
		_ = unlockFile(p.lock)
		_ = p.lock.Close()
		p.lock = nil
	}
}

// SaveProfile stores one profile under the write-back safety rule: the caller
// must already hold a PendingWrite proving the destination writable.
func (s *Store) SaveProfile(w *PendingWrite, name string, p Profile) error {
	c, err := s.Load()
	if err != nil {
		return err
	}
	if c.Profiles == nil {
		c.Profiles = map[string]Profile{}
	}
	c.Profiles[name] = p
	return s.commit(w, c)
}

// DeleteProfile removes one profile.
func (s *Store) DeleteProfile(w *PendingWrite, name string) error {
	c, err := s.Load()
	if err != nil {
		return err
	}
	delete(c.Profiles, name)
	return s.commit(w, c)
}

func (s *Store) commit(w *PendingWrite, c Credentials) error {
	c.Version = CredentialsVersion
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return localErr(err, "the credentials could not be encoded")
	}
	return w.Commit(append(data, '\n'))
}

// CredentialSource names where the credential in use came from, for
// `--verbose` and for an error message that has to say what to fix.
type CredentialSource string

const (
	// SourceEnvAccessToken is AGENT_GM_ACCESS_TOKEN: one access token, used
	// as given, not refreshable.
	SourceEnvAccessToken CredentialSource = "AGENT_GM_ACCESS_TOKEN"
	// SourceRefreshFile is AGENT_GM_REFRESH_TOKEN_FILE + AGENT_GM_CLIENT_ID:
	// automation. Every invocation exchanges the token and rewrites the file
	// with the rotated value.
	SourceRefreshFile CredentialSource = "AGENT_GM_REFRESH_TOKEN_FILE"
	// SourceProfile is the stored profile, written by `agm auth login`.
	SourceProfile CredentialSource = "profile"
	// SourceNone is no credential at all.
	SourceNone CredentialSource = "none"
)

// Credential is the resolved answer to "which server, with what".
type Credential struct {
	Server  string
	Token   string
	Source  CredentialSource
	Profile string
	// RefreshTokenFile and ClientID are set for SourceRefreshFile, whose
	// exchange happens at the start of the invocation.
	RefreshTokenFile string
	ClientID         string
	// Refreshable reports whether the token can be exchanged for a new one.
	// AGENT_GM_ACCESS_TOKEN is not: it is used as given (spec section 11.5).
	Refreshable bool
}

// ResolveCredential applies the precedence table of spec section 11.5, in
// order.
//
// The server comes from --server, else AGENT_GM_URL, else the profile's
// server. The credential comes from AGENT_GM_ACCESS_TOKEN, else the refresh
// token file, else the stored profile. A profile is bound to an exact server:
// when --server names a different one, the profile for THAT server is used
// and no token is forwarded to a new origin.
func ResolveCredential(store *Store, getenv func(string) string, serverFlag, profileFlag string) (Credential, error) {
	name := profileFlag
	if name == "" {
		name = getenv("AGENT_GM_PROFILE")
	}
	if name == "" {
		name = DefaultProfile
	}

	creds, err := store.Load()
	if err != nil {
		return Credential{}, err
	}
	profile, hasProfile := creds.Profiles[name]

	server := serverFlag
	if server == "" {
		server = getenv("AGENT_GM_URL")
	}
	if server == "" && hasProfile {
		server = profile.Server
	}
	server = strings.TrimRight(server, "/")

	c := Credential{Server: server, Profile: name, Source: SourceNone}

	if tok := getenv("AGENT_GM_ACCESS_TOKEN"); tok != "" {
		c.Token = tok
		c.Source = SourceEnvAccessToken
		c.Refreshable = false
		return c, nil
	}

	refreshFile := getenv("AGENT_GM_REFRESH_TOKEN_FILE")
	clientID := getenv("AGENT_GM_CLIENT_ID")
	if refreshFile != "" {
		if clientID == "" {
			return c, &LocalError{Msg: "AGENT_GM_REFRESH_TOKEN_FILE is set without " +
				"AGENT_GM_CLIENT_ID; the pair is the automation credential of spec 11.5 and " +
				"neither half works alone"}
		}
		c.Source = SourceRefreshFile
		c.RefreshTokenFile = refreshFile
		c.ClientID = clientID
		c.Refreshable = true
		return c, nil
	}

	if hasProfile {
		// The binding is to an exact server. A --server that names another
		// origin selects that origin's profile, or none at all.
		if server != "" && strings.TrimRight(profile.Server, "/") != server {
			if other, otherName, ok := profileForServer(creds, server); ok {
				c.Token = other.AccessToken
				c.Profile = otherName
				c.Source = SourceProfile
				c.Refreshable = other.RefreshToken != ""
				return c, nil
			}
			return c, nil
		}
		c.Token = profile.AccessToken
		c.Source = SourceProfile
		c.Refreshable = profile.RefreshToken != ""
		return c, nil
	}

	if server != "" {
		if other, otherName, ok := profileForServer(creds, server); ok {
			c.Token = other.AccessToken
			c.Profile = otherName
			c.Source = SourceProfile
			c.Refreshable = other.RefreshToken != ""
		}
	}
	return c, nil
}

func profileForServer(creds Credentials, server string) (Profile, string, bool) {
	names := make([]string, 0, len(creds.Profiles))
	for n := range creds.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if strings.TrimRight(creds.Profiles[n].Server, "/") == server {
			return creds.Profiles[n], n, true
		}
	}
	return Profile{}, "", false
}

// ReadRefreshTokenFile reads the automation credential.
func ReadRefreshTokenFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", localErr(err, "AGENT_GM_REFRESH_TOKEN_FILE %s could not be read", path)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", &LocalError{Msg: fmt.Sprintf(
			"AGENT_GM_REFRESH_TOKEN_FILE %s is empty; log in again to write one", path)}
	}
	return token, nil
}
