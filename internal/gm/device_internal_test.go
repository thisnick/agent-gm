package gm

import (
	"testing"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/util"
)

// The owner's phone must list "Agent GM", not "libgm".
//
// Left alone, libgm names itself in the paired-devices list -- upstream's own
// configuration documents `BrowserDetails.OS` as "the name that shows up in
// the paired devices list", and assertion 22 of the fixture-validation job
// pins every premise of that against the upstream tree at the pin.
func TestTheDeviceNameIsRenderedFromTheVersion(t *testing.T) {
	for version, want := range map[string]string{
		"1.0.0":              "Agent GM 1.0",
		"1.0.3":              "Agent GM 1.0",
		"v2.11.0":            "Agent GM 2.11",
		"1.0.0-slice2":       "Agent GM 1.0",
		"1.0.0+abc1234":      "Agent GM 1.0",
		"1.0.0-slice2+abc12": "Agent GM 1.0",
		"  1.4.9  ":          "Agent GM 1.4",
		// Anything unreadable degrades to the bare name. The owner reads this
		// on their phone; they cannot be shown a rendering artefact.
		"":         "Agent GM",
		"dev":      "Agent GM",
		"1":        "Agent GM",
		"1.x":      "Agent GM",
		"unknown":  "Agent GM",
		"..":       "Agent GM",
		"-slice2":  "Agent GM",
		"1..0":     "Agent GM",
		"v":        "Agent GM",
		"1.0.0.0.": "Agent GM 1.0",
	} {
		if got := DeviceOSName(version); got != want {
			t.Errorf("DeviceOSName(%q) = %q, want %q", version, got, want)
		}
	}
}

// TestThePairingRequestCarriesTheAgentGMName is the claim that matters: not
// that a helper returns a string, but that the message libgm sends to Google
// carries it.
//
// The container is built exactly as `sendGaiaPairingMessage` builds it
// (`pkg/libgm/pair_google.go:489-491`), so this fails if the override stops
// reaching the field the pairing request reads.
func TestThePairingRequestCarriesTheAgentGMName(t *testing.T) {
	if before := util.BrowserDetailsMessage.OS; before != "libgm" {
		t.Logf("BrowserDetailsMessage.OS was already %q before this test", before)
	}
	SetDeviceIdentity("1.0.0")

	if got := DeviceIdentity(); got != "Agent GM 1.0" {
		t.Fatalf("DeviceIdentity() = %q, want %q", got, "Agent GM 1.0")
	}
	if util.BrowserDetailsMessage.OS == "libgm" {
		t.Fatal("the phone would still be told `libgm`")
	}

	req := &gmproto.GaiaPairingRequestContainer{
		BrowserDetails: util.BrowserDetailsMessage,
	}
	if req.BrowserDetails.GetOS() != "Agent GM 1.0" {
		t.Errorf("the gaia pairing request would tell the phone %q, want %q",
			req.BrowserDetails.GetOS(), "Agent GM 1.0")
	}

	// Once, and only once: the variable is shared by every client in the
	// process, so a second caller must not be able to rename a device
	// mid-flight.
	SetDeviceIdentity("9.9.9")
	if got := DeviceIdentity(); got != "Agent GM 1.0" {
		t.Errorf("a second SetDeviceIdentity changed the name to %q", got)
	}
}
