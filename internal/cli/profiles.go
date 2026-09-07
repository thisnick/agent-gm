package cli

// `agm profiles` -- the machine's saved servers, and which one is in use.
//
// A profile is named for the server it belongs to and is created only by
// `agm auth login`. These three commands do not create one: they list what is
// there, choose among them, and forget one. All three are LOCAL -- they drive
// no route, so they work on a machine whose server is down, which is exactly
// when an owner wants to know what their machine is pointed at.

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// profileStore is the credentials store for a command that did not go through
// connect (a `profiles` command drives no route, so nothing resolved one).
func (r *runner) profileStore() *Store {
	if r.store != nil {
		return r.store
	}
	return NewStore(CredentialsPath(r.g.credentialsFile, r.env.Getenv))
}

// profilesList prints every stored profile and MARKS the active one. The
// marker is the point of the command: "which server would `agm health` talk
// to right now" is not answerable from the file's key order.
func (r *runner) profilesList(inv *invocation) error {
	creds, err := r.profileStore().Load()
	if err != nil {
		return err
	}
	names := profileNames(creds)

	if r.out.JSON() {
		items := make([]map[string]any, 0, len(names))
		for _, n := range names {
			items = append(items, map[string]any{
				"name":   n,
				"server": creds.Profiles[n].Server,
				"active": n == creds.ActiveProfile,
			})
		}
		data, err := json.Marshal(map[string]any{
			"items":          items,
			"active_profile": creds.ActiveProfile,
		})
		if err != nil {
			return &ContractError{Msg: "the profiles could not be encoded"}
		}
		return r.out.Emit(&Response{Status: http.StatusOK, Data: data, Warnings: []string{}})
	}

	if len(names) == 0 {
		r.out.Infof("no profiles are stored; log in with `agm auth login --server <url>`")
		return nil
	}
	for _, n := range names {
		marker := " "
		if n == creds.ActiveProfile {
			marker = "*"
		}
		_, _ = fmt.Fprintf(r.out.stdout, "%s %-32s %s\n", marker, n, creds.Profiles[n].Server)
	}
	return nil
}

// profilesUse switches the active profile. An unknown name is refused rather
// than recorded: a typo that is recorded points the machine at nothing and is
// discovered on the next command instead of this one.
func (r *runner) profilesUse(inv *invocation) error {
	name := inv.positional(0)
	if name == "" {
		return usageErr("`agm profiles use` takes a profile name; `agm profiles list` shows them")
	}
	store := r.profileStore()
	pending, err := store.Begin()
	if err != nil {
		return err
	}
	defer pending.Close()
	if err := store.SetActiveProfile(pending, name); err != nil {
		return err
	}
	return r.emitActive(store, "now using the profile "+name)
}

// profilesRemove forgets one profile. It does not revoke anything at the
// server -- that is `agm auth logout` -- and it never leaves `active_profile`
// naming a profile that is gone.
func (r *runner) profilesRemove(inv *invocation) error {
	name := inv.positional(0)
	if name == "" {
		return usageErr("`agm profiles remove` takes a profile name; " +
			"`agm profiles list` shows them")
	}
	store := r.profileStore()
	creds, err := store.Load()
	if err != nil {
		return err
	}
	if _, ok := creds.Profiles[name]; !ok {
		return unknownProfileErr(creds, name)
	}
	pending, err := store.Begin()
	if err != nil {
		return err
	}
	defer pending.Close()
	if err := store.DeleteProfile(pending, name); err != nil {
		return err
	}
	after, err := store.Load()
	if err != nil {
		return err
	}
	if after.ActiveProfile == "" && len(after.Profiles) > 0 {
		r.out.Infof("no profile is active now; choose one with `agm profiles use <name>`")
	}
	return r.emitActive(store, "removed the profile "+name)
}

// emitActive reports what the active profile is after a change, in whichever
// format was asked for. The message is already complete: it goes to stderr,
// and the machine-readable pair goes to stdout.
func (r *runner) emitActive(store *Store, message string) error {
	creds, err := store.Load()
	if err != nil {
		return err
	}
	if r.out.JSON() {
		data, err := json.Marshal(map[string]any{
			"active_profile": creds.ActiveProfile,
			"server":         creds.Profiles[creds.ActiveProfile].Server,
		})
		if err != nil {
			return &ContractError{Msg: "the active profile could not be encoded"}
		}
		return r.out.Emit(&Response{Status: http.StatusOK, Data: data, Warnings: []string{}})
	}
	r.out.Infof("%s", message)
	if creds.ActiveProfile != "" {
		_, _ = fmt.Fprintf(r.out.stdout, "%s\t%s\n",
			creds.ActiveProfile, creds.Profiles[creds.ActiveProfile].Server)
	}
	return nil
}
