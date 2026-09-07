package gm

import (
	"strconv"
	"strings"
	"sync"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/util"
)

// What the owner's phone calls this device in its paired-devices list.
//
// Google shows `BrowserDetails.OS`. Upstream's own configuration says so in
// as many words -- `pkg/connector/example-config.yaml:8`, "OS name to tell the
// phone. This is the name that shows up in the paired devices list" -- and its
// connector sets that exact field from `device_meta.os`
// (`pkg/connector/connector.go:42`). It is not the user agent, which Google
// does not display, and not `DeviceType` or `BrowserType`, which choose the
// icon.
//
// Left alone, `util.BrowserDetailsMessage.OS` is the literal string `"libgm"`
// (`pkg/libgm/util/config.go:22`), so the owner's phone lists the library
// rather than the thing that paired. This is the one place Agent GM writes a
// name onto a surface it does not own, and the surface is the owner's own
// phone.
//
// The library is not forked for it. `BrowserDetailsMessage` is an exported
// package-level pointer that upstream mutates the same way, so Agent GM sets
// the field before anything pairs. The pin stays `be48a58` (spec section 3.6).

// DeviceName is what a paired device is called, before the version.
const DeviceName = "Agent GM"

// DeviceOSName renders the paired-devices entry for a build version:
// "Agent GM 1.0" for `1.0.0`, `1.0.3` or `1.0.0-slice2`.
//
// Only major.minor, because the string is frozen at pairing time and never
// updated afterwards (see SetDeviceIdentity). A patch number or a prerelease
// tag on a label that outlives the build it names would be a claim about a
// running version that stopped being true at the next deploy; major.minor is
// the coarsest thing still worth telling the owner apart. A version this
// cannot read at all degrades to the bare name rather than to a label with a
// blank or a `%!s` in it, because the owner reads this on their phone and
// cannot be shown a rendering bug.
func DeviceOSName(version string) string {
	major, minor, ok := majorMinor(version)
	if !ok {
		return DeviceName
	}
	return DeviceName + " " + major + "." + minor
}

func majorMinor(version string) (string, string, bool) {
	v := strings.TrimSpace(version)
	v = strings.TrimPrefix(v, "v")
	// Drop build metadata and any prerelease tag: `1.0.0-slice2+abc1234`.
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return "", "", false
	}
	for _, p := range parts[:2] {
		if p == "" {
			return "", "", false
		}
		if _, err := strconv.Atoi(p); err != nil {
			return "", "", false
		}
	}
	return parts[0], parts[1], true
}

var deviceIdentityOnce sync.Once

// SetDeviceIdentity names this build in the phone's paired-devices list. It is
// called before any pairing can start.
//
// **It changes nothing about an account that is already paired.** The name is
// sent once, inside the pairing request, and Google keeps what it was told;
// there is no later message that renames a device. An account paired before
// this existed goes on saying `libgm` until the owner re-pairs it, and
// `docs/pairing.md` says so, because an owner who reads a release note and
// then looks at their phone would otherwise think it had not worked.
//
// Once, because `util.BrowserDetailsMessage` is a package-level pointer shared
// by every client in the process, and a second caller with a different version
// would race the first. The value does not vary within a build.
func SetDeviceIdentity(version string) {
	deviceIdentityOnce.Do(func() {
		util.BrowserDetailsMessage.OS = DeviceOSName(version)
	})
}

// DeviceIdentity reports what the phone will be told, for `GET /v1/health` and
// for the tests that assert the pairing request really carries it.
func DeviceIdentity() string { return util.BrowserDetailsMessage.OS }
