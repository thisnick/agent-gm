package store

import (
	"context"
	"testing"
)

// An owner-declared client is a row with two extra facts on it (spec section
// 9.3, migration 0009). These tests are at the store level because both facts
// are held by a WHERE clause somewhere else, and a WHERE clause is the kind of
// thing that can be deleted without a sequential test noticing.

func staticClient(id string) OAuthClient {
	return OAuthClient{
		ID: id, Name: "a declared connector",
		RedirectURIs:  []string{"https://connector.example.test/cb"},
		GrantTypes:    []string{"authorization_code", "refresh_token"},
		ResponseTypes: []string{"code"}, TokenEndpointAuthMethod: "none",
		MetadataJSON: "{}", Source: "1.2.3.4",
		Static: true, DefaultResource: true,
	}
}

// The two columns round-trip. A registration that predates migration 0009
// reads back with both false, which is what the DEFAULT 0 is for.
func TestAnOwnerDeclaredClientRoundTrips(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	if err := st.AuthzTx(ctx, func(tx *AuthzTx) error {
		return tx.CreateOAuthClient(staticClient("muse"))
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.OAuthClientByID(ctx, "muse")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Static {
		t.Error("the row came back without Static set")
	}
	if !got.DefaultResource {
		t.Error("the row came back without DefaultResource set")
	}

	// A dynamically registered row is neither, and must not become either.
	dynamic := ClientID()
	if err := st.AuthzTx(ctx, func(tx *AuthzTx) error {
		return tx.CreateOAuthClient(OAuthClient{
			ID: dynamic, RedirectURIs: []string{"http://127.0.0.1/cb"},
			GrantTypes: []string{"authorization_code"}, ResponseTypes: []string{"code"},
			TokenEndpointAuthMethod: "none", MetadataJSON: "{}",
		})
	}); err != nil {
		t.Fatal(err)
	}
	back, err := st.OAuthClientByID(ctx, dynamic)
	if err != nil {
		t.Fatal(err)
	}
	if back.Static || back.DefaultResource {
		t.Errorf("a dynamic registration came back static=%v default_resource=%v",
			back.Static, back.DefaultResource)
	}
}

// The 24-hour sweep never removes an owner-declared client.
//
// The row is written in the shape the sweep looks for -- never activated, with
// an expiry already in the past -- so the ONLY thing keeping it is the
// `static = 0` guard in ExpiredUnreferencedClients. Drop that guard and this
// test fails, which is the point: in production a declared row is also
// activated at creation, so a test built on a realistic row would pass either
// way and prove nothing.
func TestTheSweepNeverRemovesAnOwnerDeclaredClient(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	declared := staticClient("muse")
	declared.ExpiresAtMS = 1 // 1970, and long past
	declared.ActivatedAtMS = 0

	expired := ClientID()
	if err := st.AuthzTx(ctx, func(tx *AuthzTx) error {
		if err := tx.CreateOAuthClient(declared); err != nil {
			return err
		}
		return tx.CreateOAuthClient(OAuthClient{
			ID: expired, RedirectURIs: []string{"http://127.0.0.1/cb"},
			GrantTypes: []string{"authorization_code"}, ResponseTypes: []string{"code"},
			TokenEndpointAuthMethod: "none", MetadataJSON: "{}",
			ExpiresAtMS: 1,
		})
	}); err != nil {
		t.Fatal(err)
	}

	var sweepable []string
	if err := st.AuthzTx(ctx, func(tx *AuthzTx) error {
		var err error
		sweepable, err = tx.ExpiredUnreferencedClients()
		return err
	}); err != nil {
		t.Fatal(err)
	}

	for _, id := range sweepable {
		if id == "muse" {
			t.Fatal("the sweep offered to remove an owner-declared client; the owner " +
				"created it on purpose and nothing expires it")
		}
	}
	var sawDynamic bool
	for _, id := range sweepable {
		if id == expired {
			sawDynamic = true
		}
	}
	if !sawDynamic {
		t.Fatal("the sweep missed an expired dynamic registration, so this test " +
			"proves nothing about the static one")
	}
}

// An owner-declared client does not spend the /oauth/register budget. The
// budget bounds what an unknown caller can create at a public endpoint; a
// declared client came from an admin-scoped call the owner made.
func TestAnOwnerDeclaredClientIsNotCountedAgainstTheRegistrationBudget(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	if err := st.AuthzTx(ctx, func(tx *AuthzTx) error {
		return tx.CreateOAuthClient(staticClient("muse"))
	}); err != nil {
		t.Fatal(err)
	}
	n, err := st.CountRegistrationsSince(ctx, "1.2.3.4", 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("the declared client counted %d against its source's registration budget", n)
	}

	// The same source registering dynamically still counts, so the guard is
	// narrow rather than a disabled budget.
	if err := st.AuthzTx(ctx, func(tx *AuthzTx) error {
		return tx.CreateOAuthClient(OAuthClient{
			ID: ClientID(), RedirectURIs: []string{"http://127.0.0.1/cb"},
			GrantTypes: []string{"authorization_code"}, ResponseTypes: []string{"code"},
			TokenEndpointAuthMethod: "none", MetadataJSON: "{}", Source: "1.2.3.4",
		})
	}); err != nil {
		t.Fatal(err)
	}
	if n, err = st.CountRegistrationsSince(ctx, "1.2.3.4", 0); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("the registration budget counted %d dynamic registrations, want 1", n)
	}
}
