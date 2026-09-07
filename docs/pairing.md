# Pairing a Google account

Serves spec §3.2, §3.3, §3.5, §4.7, §7.2, §11.4 and §12.1.

## What pairing is, and what it is not

Agent GM talks to Google Messages the way the Messages web client does: it
signs in to your Google account, asks Google which Android device on that
account is the phone, and completes a device pairing that the phone confirms
by emoji. That is the **gaia flow**, and at this pin it is the only one.

**There is one pairing flow and no QR option.** Google withdrew QR device
pairing, so there is nothing to choose between and no `--qr` flag to look
for.

`agm pair` is one command from start to paired. It opens a browser, captures
the session, starts the pairing, prints an emoji, and waits for you to tap it
on the phone. You are not expected to find cookies yourself; the paste
fallback below exists for machines that cannot run a browser.

**What it costs you, stated once and plainly.** The gaia flow needs seven
Google session cookies, and those cookies are not consumed at pairing — they
are attached to every subsequent request and rotated in place as Google
reissues them. Agent GM therefore **holds live Google account cookies for the
whole life of the pairing**, sealed inside `sessions/<acct_id>.enc` (§3.2,
§3.3). That is the whole Google account, not a scoped token. There is no
lower-privilege alternative to pick instead; there are only handling rules,
and they are in [operations.md](operations.md).

## Before you start

`agm pair` drives the `admin`-scoped pairing routes, so you need a credential
first: `agm auth login --admin --server <url>` once, on this machine. A
`--server` no profile knows is refused on every other command, and `agm pair`
with no profile at all exits `9` saying what to type.

You need three things:

- the Google account you want to add, and its password or passkey;
- the Android phone signed in to that account, unlocked and to hand;
- Chrome or Chromium on the machine you run `agm pair` from — or a plan from
  the "when Chrome does not open" section below.

## The walkthrough

```console
$ agm pair
Adding a Google account to Agent GM.
Opening a dedicated Chrome window for Google sign-in.
This profile belongs to agm alone; your normal Chrome is untouched.

  Sign in to your Google account in that window, and wait for
  Google Messages for web to load.

  Captured 7 cookies.  Closing Chrome.

  Your phone will show three emoji and ask which one matches.
  Tap this one:

        🦋

  Waiting for the phone...
```

Step by step, and what each step is for:

1. **A Chrome window opens** at
   `https://accounts.google.com/AccountChooser?continue=https://messages.google.com/web/config`
   — Google Messages' own sign-in URL. The window belongs to a profile
   created for this one capture; your everyday Chrome, its tabs and its
   sessions are untouched.

2. **You sign in.** Because the profile is brand new and empty, Google sees
   an unrecognised device. A 2FA prompt or a device-verification challenge
   here is normal, not a failure — the command waits rather than aborting.

3. **You wait for Google Messages for web to finish loading.** This matters
   more than it looks. One of the seven cookies, `OSID`, is host-scoped to
   `messages.google.com` and is not set by the `accounts.google.com` sign-in
   alone. Agent GM waits for `OSID` to exist before it reads anything, and
   tells you what it is waiting for; reading early would return a set that
   looks complete and does not work.

4. **Chrome closes by itself** the moment the capture completes. You do not
   close it, and you do not need to sign out of it.

5. **Your phone shows a choice of emoji** in Google Messages, with a "this is
   not me" option beside them. The terminal prints one emoji, large and
   alone. Tap the matching one on the phone.

6. **Agent GM backfills.** When the pairing completes the CLI prints the
   account, the phone, and the backfill as it runs:

   ```console
   Paired.  account: acct_7f2a...  (alex@example.com)  phone: <phone_id>
   Backfilling 41 conversations... done (2,183 messages).
   ```

If this is your second or later account, the CLI also reminds you that writes
need `--account` once more than one account exists, and that
`agm accounts list` and `agm accounts label` are how you tell them apart.

## The Chrome profile is short-lived

The profile `agm pair` creates is temporary. It is made fresh
under the platform's temporary directory at mode `0700` for one capture, and
**deleted the moment Chrome closes — on success, on failure, on timeout and
on Ctrl-C**. Nothing is written under `$XDG_STATE_HOME/agent-gm/`. A
directory holding a logged-in Google session never outlives the command that
made it.

There is therefore **no kept profile and no `--forget-browser` command**:
there is nothing kept to forget. If you go looking for one because another
tool has it, it is absent on purpose.

Say the consequence plainly, because you will meet it:

- **Every capture is a new device to Google.** So 2FA, or a
  device-verification challenge, is a normal one-time cost of each capture,
  not a sign that something is wrong.
- **`agm pair --refresh-cookies` asks for a sign-in again.** Google may or
  may not also ask for the password; that is Google's decision and Agent GM
  cannot predict it. The command says so before it opens the window.

One more thing worth knowing while the window is open: **Chrome's debugging
port is a live credential channel.** Anything that can connect to it reads
every cookie in that profile. Agent GM binds it to `127.0.0.1` on a random
port and kills Chrome as soon as the capture finishes, but the window between
those two moments is real.

## When Chrome does not open

Chrome's own stderr is kept and surfaced, not swallowed. If the browser exits
before the debugging port answers, what Chrome said is the error `agm pair`
prints, naming the exit — not a vague "the debugging port never answered".

On top of that, `agm pair` adds a desktop-session hint in two cases:

- **`DISPLAY` and `WAYLAND_DISPLAY` are both unset.** There is no graphical
  session for Chrome to open a window in. The usual cause is running `agm`
  over `ssh`, in a container, or from a system service unit.
- **`DISPLAY` is set, but there is no `XAUTHORITY` and no `~/.Xauthority`.**
  Chrome is pointed at an X server it has no authority cookie for, and the X
  server refuses it. Chrome's own message here is "cannot open display",
  which nobody reads as "set `XAUTHORITY`".

The concrete Linux-desktop recipe, in order of preference:

1. **Run `agm pair` from a terminal inside the desktop session** — a terminal
   emulator you opened on the desktop itself, not an `ssh` shell into the
   same machine. This is right far more often than it sounds, because a
   headless-feeling home server frequently does have a logged-in graphical
   session on its console.

2. **Take `XAUTHORITY` from the running session.** If `DISPLAY` is set but
   the authority file is not, borrow it from a process that is part of the
   session:

   ```console
   $ export XAUTHORITY=$(tr '\0' '\n' < /proc/$(pgrep -x gnome-shell | head -1)/environ \
       | sed -n 's/^XAUTHORITY=//p')
   $ agm pair
   ```

   Substitute your compositor's process name for `gnome-shell` if you run
   something else.

3. **Pair from a machine that has a desktop**, pointing at this server — see
   "Pairing a headless server" below.

4. **Use the paste fallback** if the machine genuinely has no browser.

If no Chrome or Chromium binary is found at all — `$AGENT_GM_CHROME` first,
then the platform's usual locations, then `PATH` — `agm pair` does not fail
with a missing-binary error. It explains what it needs and offers the ways
forward, and exits **9** (local configuration), not 2: nothing about your
command was malformed. Set `AGENT_GM_CHROME` to a binary path if Chrome is
somewhere unusual.

## Pasting the cookies yourself

`agm pair --paste` is the documented fallback for a machine with no Chrome at
all.

```console
$ agm pair --paste                 # reads the paste from stdin
$ agm pair --paste-file ./curl.txt
```

**What to copy.** In a browser that is already signed in to
`messages.google.com`, open devtools, go to the **Network** panel, find a
request **to `messages.google.com`**, right-click it and choose **Copy as
cURL**. Paste that. Agent GM also accepts a JSON object of cookie name to
value, matching upstream's own instruction text.

**Copy a request to `messages.google.com`, not to `.google.com`.** This is
the one thing to get right. `OSID` is host-scoped to `messages.google.com`,
so a copy taken from a `.google.com` request silently omits it and the
resulting set is unusable. If the paste is missing a required cookie the CLI
names the missing ones and the domain each comes from, rather than failing
later with something opaque.

The seven cookies, and where each lives:

| Cookie | Domain | Required |
|---|---|---|
| `SID` | `.google.com` | yes |
| `HSID` | `.google.com` | yes |
| `SSID` | `.google.com` | yes |
| `APISID` | `.google.com` | yes |
| `SAPISID` | `.google.com` | yes |
| `OSID` | **`messages.google.com`** | yes |
| `__Secure-1PSIDTS` | `.google.com` | no |

Five of the seven — `SID`, `HSID`, `SSID`, `OSID` and `__Secure-1PSIDTS` —
are `httpOnly`. That is why there is no bookmarklet, no console snippet and
no `document.cookie` trick: JavaScript cannot see them. Only the browser's
own cookie store, read through devtools or through the debugging protocol,
produces a usable set.

**The paste is never echoed and never written to disk.** It is parsed once,
in memory, and only the seven cookies are kept — a devtools copy carries far
more, and none of the rest is stored. Nothing you paste appears in `argv`, in
a log line, in an audit payload or in an error message.

## Pairing a headless server

If Agent GM runs somewhere with no desktop, run `agm pair` on a machine that
has one and point it at the server:

```console
$ agm pair --server https://gm.example.test
```

Chrome runs on your laptop; the CLI sends **only the seven cookies** to the
server over TLS. Nothing else about your laptop travels.

Stated plainly, because it is the trust you are extending:

> **The server then holds live Google account cookies for the whole life of
> the pairing**, inside `sessions/<acct>.enc`. Not "saw them once" — *holds
> them*, and rotates them in place as Google reissues them. Anyone with the
> data directory **and** `AGENT_GM_DATA_KEY` has the owner's Google account.

That is why session files are mode `0600` in a `0700` directory, why every
one of the seven cookies is redacted by name from every log and audit
payload, and why a backup of `sessions/` is handled as a credential backup
(see [operations.md](operations.md)).

## What your phone calls this device

In the Messages app, under **Device pairing**, the paired-devices list shows
one entry per connected device. Agent GM appears there as **`Agent GM 1.0`** —
the name and the major.minor version of the build that paired.

The name is sent once, inside the pairing request, and Google keeps what it
was told. There is no later message that renames a device, which has two
consequences worth knowing before you go looking for a setting:

- **An account paired before this existed keeps its old label.** Earlier
  builds sent the underlying library's own name, `libgm`, so an account paired
  by one of them still says `libgm` on your phone. Nothing is wrong with it
  and it goes on working. It changes only when that account is **re-paired**
  (`agm pair`), which is not worth doing for the label alone.
- **The version in the label does not track upgrades.** It is the version that
  paired, frozen. That is also why only major.minor is sent: a patch number on
  a label that outlives the build it names would be a claim that stopped being
  true at the next deploy.

If you have paired more than once — a re-pair, or a second Google account —
each entry is its own row, and the `dest_reg_uuid` that `agm pair` printed is
what tells them apart with certainty. The label is for reading; the UUID is
for identifying.

## Several Android devices on one Google account

Google's device enumeration is not "the single primary". If your Google
account has more than one Android device, the library sorts them by
last-seen, newest first, and picks the most recently seen. It does not ask
and it does not error.

If it picked the wrong phone, select the next one:

```console
$ agm pair --device-index 1
```

`agm pair` prints the **`dest_reg_uuid`** of the device it chose, and records
the same value in the audit row, so a second run can be compared against the
first and "which phone did we pair?" stays answerable afterwards.

**It prints that and nothing more: no last-seen timestamp, and no device
count.** Agent GM drives pairing through `DoGaiaPairing`, which returns
neither the pairing session nor the candidate list, so those two values reach
only an upstream log line. For the same reason `pairing_init_timeout` carries
`details.multiple_devices: true` but never a `details.device_count`.

## Two settings to check on the phone, once

Both are on the phone, in Google Messages, and both are checked once per
phone rather than per pairing.

1. **Google Messages must be the default SMS app.** If it is not, every send
   fails. `agm health` reports this per account as `is_default_sms_app`, and
   a send that trips it is the `not_default_sms_app` error code. Agent GM
   cannot change the setting; only you can, on the phone.

2. **Group messaging must be "Send an MMS reply to all recipients."**
   Settings → Advanced → Group messaging. With the SMS setting instead, a
   group send may fan out as separate one-to-one SMS threads rather than
   staying one group conversation. Agent GM can only report what Google
   returns; it cannot detect the setting directly.

Using Google Messages for web in a browser at the same time as Agent GM is
fine. It causes extra resyncs — the phone marks one session inactive and the
other active, and each switch triggers a resync — which costs bandwidth and
backfill churn, not the pairing.

## Re-pairing, signing out, and removing

These three are different acts with very different consequences.

**Signing out keeps everything.** `agm accounts sign-out <acct-id>`
disconnects that account, shreds its `sessions/<acct>.enc` and zeroes the
in-memory session — so the Google account cookies are gone from disk and from
the process — sets the account to `signed_out`, and writes an audit row. It
deletes **no** conversation, message, attachment, reaction, contact or
operation. Everything stays readable and searchable; the account simply
appears in listings with `state: "signed_out"`, and writes naming it are
refused with `unsupported_capability` and `details.reason = "not_signed_in"`.
Cookie expiry and a pairing revoked from the phone have the same effect, and
also delete nothing.

**Removing is the only thing that deletes.** `agm accounts remove <acct-id>`
requires an interactive `y` or `--yes`, prints its exact effect sentence

> *"permanently deletes Agent GM's copy of this account's conversations,
> messages, attachments and operations; your Google Messages account and the
> messages in it are untouched"*

signs the account out first, then deletes its rows in one transaction and
unlinks its cached media afterwards. Audit rows are **not** deleted — they
record what happened when it happened, and they keep their `account_id`, so
the trail of a removed account survives it.

**Re-pairing resumes the same account.** Run `agm pair` again for an account
you already have and Agent GM recognises it and reuses the same `acct_` ID.
The identifier is derived from the **Google account address** rather than from
the phone, so re-pairing keeps every existing `conv_` and
`msg_` ID **even onto a different phone**, which is the whole reason the
identifier was chosen that way. History is not duplicated: ingest is an
upsert on Google's own stable IDs, so backfill after a re-pair rewrites
nothing it already has. The account returns to `connected`, a reconciliation
sweep runs, and messages that arrived while it was signed out are ingested
normally.

Re-pairing an account that already exists **does not** move it to `pairing`.
An account that is `signed_out`, `error` or `parked` keeps that state for the
whole of the new pairing and flips straight to `connected` on success, so
nothing watching it ever sees it become less usable than it already was. Only
a genuinely new account passes through `pairing`.

Two flags make intent explicit:

- `agm pair --account <acct-id>` asserts which account you expect. If the
  address you sign in as does not match, the pairing is refused with
  `pairing_wrong_account` and nothing changes.
- `agm pair --refresh-cookies --account <acct-id>` re-authenticates an
  existing pairing with fresh cookies. **Cookie expiry does not require a
  re-pair**: the pairing survives, and this is the fix. A capture from a
  different Google account is refused with `pairing_wrong_account`. Whether
  `--refresh-cookies` may be combined with `--paste` on a machine with no
  Chrome is **not specified**; the spec describes the refresh only as a fresh
  Chrome capture.

If a Google account's owner renames it, Agent GM sees a new account, because
the address is the identity. A re-pair then creates a **second** account. It
never adopts the first one's rows. Confirm what happened, then
`agm accounts remove` the stale one.

Agent GM never calls Google's unpair. Signing out is a local act; Google
keeps the pairing until the phone or the Google account removes it. And Agent
GM never pairs implicitly — `agm pair` is always explicit, always interactive
or `--yes`-gated, and always writes an audit record.

## When pairing fails

Every failure below is an HTTP `409` from `POST /v1/pairing/start` and exit
code `10` from the CLI, except `pairing_init_timeout`, which is retryable and
exits `7`.

| Code | What happened | What to do |
|---|---|---|
| `pairing_no_cookies` | the pairing was attempted with no cookies at all | for `--paste`, check that you actually pasted something; for the Chrome flow this should not happen and is worth reporting |
| `pairing_no_devices` | Google reports no primary Android device on this account | check that the phone is signed in to this Google account and has Google Messages set up, then retry. The spec does not enumerate the conditions under which Google stops listing a device |
| `pairing_wrong_emoji` | the emoji tapped on the phone was not the one printed | run `agm pair` again and read the terminal carefully; the phone shows three, only one matches |
| `pairing_cancelled` | "this is not me" was tapped, or the prompt was dismissed | run `agm pair` again and confirm on the phone |
| `pairing_timeout` | Google's own pairing window expired before the phone answered | have the phone unlocked and Google Messages open before you start, then retry |
| `pairing_init_timeout` | the initial round trip to Google exceeded 20 seconds. **Retryable.** Carries `details.multiple_devices: true` when the account had more than one candidate device — there is no device count | retry. If it recurs and `multiple_devices` is true, try `--device-index 1` |
| `pairing_wrong_account` | a `--refresh-cookies` or `--account` run signed in as a different Google account | sign in as the account you named, or drop `--account` to pair the new address as a second account |
| `pairing_no_account` | Google returned no usable account address, so there is no identity to key the account on | nothing was created — no account row and no session file. Retry; if it persists, report it |

Two failures are not pairing errors at all and are worth telling apart:

- **`agm pair` could not find Chrome** — exit `9`, local configuration. See
  "when Chrome does not open".
- **The OSID cookie never appeared** — the sign-in never reached
  `messages.google.com`. Run it again and let Google Messages for web finish
  loading before Chrome closes.

A pairing that is started and never confirmed does not linger. The half-built
session is discarded and the placeholder row deleted when the emoji is wrong
or cancelled, when the session is signed out, when
`AGENT_GM_PAIRING_TIMEOUT` (default `5m`) elapses, when you Ctrl-C — which
issues the abandon — or when the process restarts. `pairing` is never a
resting state.
