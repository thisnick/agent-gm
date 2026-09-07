package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/thisnick/agent-gm/internal/apierr"
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
	// LegacyDefaultProfile is the name every profile used to be given, back
	// when there was one unnamed profile per machine. There is no such thing
	// now: a profile is named for the server it belongs to, and a file
	// carrying this name is migrated on read (see migrate).
	LegacyDefaultProfile = "default"
)

// noServerMessage is the ONE sentence the CLI has for "I do not know which
// server to talk to". There is deliberately no built-in hostname anywhere in
// this package -- not as a fallback, not in an example, not in this message:
// a CLI that silently reaches a compiled-in deployment is a CLI that can send
// one owner's request to another owner's server.
const noServerMessage = "no server is configured: log in with `agm auth login --server <url>`, " +
	"or pass --server or set AGENT_GM_URL for a server you have already logged in to"

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
	// ClientID is the dynamic registration this profile's tokens belong to.
	// It is stored because the OAuth refresh grant requires it (spec section
	// 9.6) and because a profile that could not name its client would have
	// to register a second one on every refresh.
	ClientID string `json:"client_id,omitempty"`
}

// Credentials is the whole file.
//
// ActiveProfile is which profile a command with no `--profile` uses. It is
// recorded by `agm auth login` and changed by `agm profiles use`; there is no
// implicit "default" profile to fall back on, because a fallback that picks a
// profile for you is a fallback that eventually picks the wrong server.
type Credentials struct {
	Version       int                `json:"version"`
	ActiveProfile string             `json:"active_profile,omitempty"`
	Profiles      map[string]Profile `json:"profiles"`
}

// CredentialsVersion is the on-disk format version. It stays at 1:
// `active_profile` is an added field and the profile rename happens on read,
// so a file written by an older build is read by this one without a version
// bump, and a file written by this one is still read by an older build (which
// simply ignores the field it does not know).
const CredentialsVersion = 1

// ProfileNameForServer is the name `agm auth login` gives a profile when
// `--profile` is not passed: the server's host, so that `agm profiles list`
// reads as the list of servers this machine can talk to.
func ProfileNameForServer(server string) (string, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(server), "/")
	if trimmed == "" {
		return "", &LocalError{Msg: noServerMessage}
	}
	u, err := url.Parse(trimmed)
	if err != nil || u.Host == "" {
		return "", &apierr.Error{Code: apierr.CodeInvalidRequest, Message: fmt.Sprintf(
			"%q is not a server URL: it needs a scheme and a host, as in "+
				"`agm auth login --server https://gm.example.test`", server)}
	}
	return u.Host, nil
}

// migrate brings a loaded file forward, idempotently.
//
// Two rules, both of which have to hold on every read rather than once at
// some upgrade moment, because the file is shared with older builds and with
// an operator's editor:
//
//   - a profile literally named "default" is renamed to its server's host and
//     becomes the active profile. That name meant "the one profile", and the
//     one profile is exactly what no longer exists;
//   - a file with no active_profile and exactly one profile treats that one
//     as active. Anything else would make a working single-server install
//     stop working on upgrade.
//
// Every other field is preserved untouched.
func migrate(c Credentials) Credentials {
	legacy, ok := c.Profiles[LegacyDefaultProfile]
	if ok {
		name, err := ProfileNameForServer(legacy.Server)
		switch {
		case err != nil || name == LegacyDefaultProfile:
			// A "default" profile that names no usable server cannot be
			// renamed. It is left alone rather than dropped: it is the
			// operator's file, and losing a token to a migration is worse
			// than carrying an odd name.
		default:
			existing, taken := c.Profiles[name]
			if !taken {
				c.Profiles[name] = legacy
				delete(c.Profiles, LegacyDefaultProfile)
			} else if strings.TrimRight(existing.Server, "/") == strings.TrimRight(legacy.Server, "/") {
				// Already migrated once; the named profile is authoritative.
				delete(c.Profiles, LegacyDefaultProfile)
			}
			if c.ActiveProfile == "" || c.ActiveProfile == LegacyDefaultProfile {
				c.ActiveProfile = name
			}
		}
	}
	if _, live := c.Profiles[c.ActiveProfile]; c.ActiveProfile != "" && !live {
		c.ActiveProfile = ""
	}
	if c.ActiveProfile == "" && len(c.Profiles) == 1 {
		for only := range c.Profiles {
			c.ActiveProfile = only
		}
	}
	return c
}

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
	return migrate(onDisk), nil
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
// must already hold a PendingWrite proving the destination writable. The
// profile it stores becomes the active one, because storing a credential the
// next command would not use is not what anyone typing `agm auth login`
// means.
func (s *Store) SaveProfile(w *PendingWrite, name string, p Profile) error {
	c, err := s.Load()
	if err != nil {
		return err
	}
	if c.Profiles == nil {
		c.Profiles = map[string]Profile{}
	}
	c.Profiles[name] = p
	c.ActiveProfile = name
	return s.commit(w, c)
}

// DeleteProfile removes one profile. It never leaves `active_profile` naming
// a profile that is no longer there: removing the active one leaves the
// remaining profile active when exactly one remains, and otherwise leaves no
// active profile at all, so the next command says so rather than guessing.
func (s *Store) DeleteProfile(w *PendingWrite, name string) error {
	c, err := s.Load()
	if err != nil {
		return err
	}
	delete(c.Profiles, name)
	if c.ActiveProfile == name {
		c.ActiveProfile = ""
		if len(c.Profiles) == 1 {
			for only := range c.Profiles {
				c.ActiveProfile = only
			}
		}
	}
	return s.commit(w, c)
}

// SetActiveProfile is `agm profiles use`. A name that is not stored is
// refused: it is exactly the typo that would otherwise leave the machine
// pointed at nothing.
func (s *Store) SetActiveProfile(w *PendingWrite, name string) error {
	c, err := s.Load()
	if err != nil {
		return err
	}
	if _, ok := c.Profiles[name]; !ok {
		return unknownProfileErr(c, name)
	}
	c.ActiveProfile = name
	return s.commit(w, c)
}

// unknownProfileErr names what is actually stored, because a profile name is
// a host and a host is easy to mistype.
func unknownProfileErr(c Credentials, name string) error {
	msg := fmt.Sprintf("there is no profile named %q", name)
	if known := profileNames(c); len(known) > 0 {
		msg += "; this machine has " + strings.Join(known, ", ")
	} else {
		msg += "; this machine has none. Log in with `agm auth login --server <url>`"
	}
	return &apierr.Error{Code: apierr.CodeInvalidRequest, Message: msg}
}

func profileNames(c Credentials) []string {
	names := make([]string, 0, len(c.Profiles))
	for n := range c.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func (s *Store) commit(w *PendingWrite, c Credentials) error {
	// migrate on the way out as well as on the way in, so the invariants it
	// enforces -- no profile named "default", never an active_profile naming
	// a profile that is not there, and a lone profile is the active one --
	// hold for whatever reads the file next, including an older build.
	c = migrate(c)
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

// ResolveCredential applies the precedence of spec section 11.5, in order.
//
// The server comes from `--server`, else `AGENT_GM_URL`, else the ACTIVE
// profile, and from nowhere else. There is no built-in hostname: when none of
// the three resolves, the command fails naming what to type.
//
// The profile comes from `--profile`, else `AGENT_GM_PROFILE`, else
// `active_profile` in the credentials file. There is no implicit "default".
//
// The credential comes from AGENT_GM_ACCESS_TOKEN, else the refresh token
// file, else the stored profile.
//
// A profile is bound to an exact server, and on a command that is not
// `agm auth login` a `--server` naming a server no profile holds is REFUSED
// rather than attempted: the alternative is a request sent to an origin this
// machine has no credential for, which fails later and less clearly, or --
// worse -- succeeds against the wrong server. `creating` is true only for
// `agm auth login`, which is where a profile comes from and therefore the one
// command allowed to name a server it has never seen.
func ResolveCredential(store *Store, getenv func(string) string, serverFlag, profileFlag string, creating bool) (Credential, error) {
	creds, err := store.Load()
	if err != nil {
		return Credential{}, err
	}

	requested := profileFlag
	if requested == "" {
		requested = getenv("AGENT_GM_PROFILE")
	}

	var (
		name       string
		profile    Profile
		hasProfile bool
	)
	if requested != "" {
		name = requested
		profile, hasProfile = creds.Profiles[requested]
		if !hasProfile && !creating {
			return Credential{}, unknownProfileErr(creds, requested)
		}
	} else if creds.ActiveProfile != "" {
		name = creds.ActiveProfile
		profile, hasProfile = creds.Profiles[name]
	}

	server := strings.TrimRight(strings.TrimSpace(serverFlag), "/")
	if server == "" {
		server = strings.TrimRight(strings.TrimSpace(getenv("AGENT_GM_URL")), "/")
	}
	explicitServer := server != ""
	if server == "" && hasProfile {
		server = strings.TrimRight(profile.Server, "/")
	}

	c := Credential{Server: server, Profile: name, Source: SourceNone}

	// `agm auth login` names its own profile: --profile, else the server's
	// host. It is the only command that may reach a server no profile holds.
	if creating {
		if server == "" {
			return c, &LocalError{Msg: noServerMessage}
		}
		if requested == "" {
			derived, err := ProfileNameForServer(server)
			if err != nil {
				return c, err
			}
			name = derived
		}
		c.Profile = name
		if tok := getenv("AGENT_GM_ACCESS_TOKEN"); tok != "" {
			c.Token = tok
			c.Source = SourceEnvAccessToken
		}
		return c, nil
	}

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

	// From here the credential can only be a stored profile.
	use := func(p Profile, n string) Credential {
		c.Token = p.AccessToken
		c.Profile = n
		c.Source = SourceProfile
		c.Refreshable = p.RefreshToken != ""
		return c
	}

	if !explicitServer {
		if hasProfile {
			return use(profile, name), nil
		}
		return c, nil
	}

	if hasProfile && strings.TrimRight(profile.Server, "/") == server {
		return use(profile, name), nil
	}
	if other, otherName, ok := profileForServer(creds, server); ok {
		return use(other, otherName), nil
	}
	return c, &apierr.Error{Code: apierr.CodeInvalidRequest, Message: fmt.Sprintf(
		"this machine holds no profile for the server %s, and a profile's token is never "+
			"forwarded to a server it was not issued for. Log in to it with "+
			"`agm auth login --server %s`%s", server, server, storedProfilesSuffix(creds))}
}

func storedProfilesSuffix(c Credentials) string {
	known := profileNames(c)
	if len(known) == 0 {
		return ""
	}
	return "; this machine has " + strings.Join(known, ", ")
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
