// The MCP conformance run of spec section 8.4.
//
// This script mints a token **through the whole OAuth flow** -- admin
// bootstrap, enrollment code, dynamic registration, the authorization screen,
// owner approval, the token endpoint -- starts a small loopback proxy that
// adds it, and runs the pinned `@modelcontextprotocol/conformance` package
// against the proxy.
//
// An admin bootstrap token is deliberately NOT used for the run itself. It
// would work, and it would leave the client-shaped path unmeasured: the whole
// reason section 8.4 asks for the full flow is that the flow is what claude.ai
// and ChatGPT perform, and a green conformance run over a credential no
// connector can obtain proves nothing about them. A failure to complete the
// flow is announced loudly here rather than silently downgraded.
//
// The baseline (`scripts/mcp-conformance-baseline.yaml`) is checked in BOTH
// directions: a new failure fails the run, and so does a listed scenario that
// starts passing. Without the second direction the file rots into a list of
// excuses nobody revisits.

import { spawn } from "node:child_process";
import { createServer } from "node:http";
import { readFileSync } from "node:fs";
import { request as httpRequest } from "node:http";

const SERVER = process.env.AGENT_GM_CONFORMANCE_SERVER; // http://127.0.0.1:PORT
const ADMIN_SECRET = process.env.AGENT_GM_ADMIN_SECRET;
const PACKAGE = process.env.AGENT_GM_CONFORMANCE_PACKAGE;
const BASELINE = process.env.AGENT_GM_CONFORMANCE_BASELINE;
const SUITE = process.env.AGENT_GM_CONFORMANCE_SUITE || "active";

if (!SERVER || !ADMIN_SECRET || !PACKAGE || !BASELINE) {
  die(
    "conformance: AGENT_GM_CONFORMANCE_SERVER, AGENT_GM_ADMIN_SECRET, " +
      "AGENT_GM_CONFORMANCE_PACKAGE and AGENT_GM_CONFORMANCE_BASELINE are all required",
  );
}

function die(message) {
  console.error(message);
  process.exit(1);
}

/** One HTTP request that never follows a redirect: every redirect in the
 *  OAuth flow is something this script has to read. */
async function fetchNoRedirect(url, init = {}) {
  return fetch(url, { ...init, redirect: "manual" });
}

function formBody(fields) {
  return new URLSearchParams(fields).toString();
}

/** Pull the hidden inputs out of the approval page, which is what a browser
 *  would submit back. */
function hiddenFields(html) {
  const out = {};
  const re = /<input type="hidden" name="([a-z_]+)" value="([^"]*)">/g;
  let m;
  while ((m = re.exec(html)) !== null) {
    out[m[1]] = m[2]
      .replaceAll("&amp;", "&")
      .replaceAll("&lt;", "<")
      .replaceAll("&gt;", ">")
      .replaceAll("&#34;", '"')
      .replaceAll("&#39;", "'");
  }
  if (!out.context || !out.form_token) {
    throw new Error(
      "the authorization screen carried no signed context or form token:\n" + html,
    );
  }
  return out;
}

function base64url(buf) {
  return Buffer.from(buf).toString("base64url");
}

async function pkce() {
  const { randomBytes, createHash } = await import("node:crypto");
  const verifier = base64url(randomBytes(32));
  const challenge = base64url(createHash("sha256").update(verifier).digest());
  return { verifier, challenge };
}

/** step announces each stage, so a failure names the stage that failed rather
 *  than a status code with no story. */
function step(name) {
  process.stderr.write(`conformance: ${name}\n`);
}

async function mintTokenThroughTheWholeFlow() {
  step("discovery");
  const metadata = await (await fetch(`${SERVER}/.well-known/oauth-authorization-server`)).json();
  const resourceDoc = await (
    await fetch(`${SERVER}/.well-known/oauth-protected-resource/mcp`)
  ).json();
  if (metadata.issuer !== SERVER) {
    throw new Error(`issuer is ${metadata.issuer}, want ${SERVER} byte for byte`);
  }
  const resource = resourceDoc.resource;

  step("admin bootstrap");
  const session = await (
    await fetch(`${SERVER}/v1/auth/admin-session`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ secret: ADMIN_SECRET }),
    })
  ).json();
  const admin = session?.data?.access_token;
  if (!admin) throw new Error("the admin bootstrap returned no access token");

  step("enrollment code");
  const enrollment = await (
    await fetch(`${SERVER}/v1/admin/enrollment-codes`, {
      method: "POST",
      headers: { "Content-Type": "application/json", Authorization: `Bearer ${admin}` },
      body: JSON.stringify({
        label: "mcp conformance",
        scopes: ["messages:read", "messages:write", "messages:delete"],
      }),
    })
  ).json();
  const code = enrollment?.data?.code;
  if (!code) throw new Error("no enrollment code was issued");

  step("dynamic client registration");
  const registration = await (
    await fetch(`${SERVER}/oauth/register`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        client_name: "mcp conformance",
        redirect_uris: ["http://127.0.0.1/conformance-callback"],
      }),
    })
  ).json();
  const clientID = registration.client_id;
  if (!clientID) throw new Error("registration returned no client_id");

  step("the authorization screen");
  const { verifier, challenge } = await pkce();
  const authorizeURL =
    `${SERVER}/oauth/authorize?` +
    new URLSearchParams({
      response_type: "code",
      client_id: clientID,
      redirect_uri: "http://127.0.0.1/conformance-callback",
      state: "conformance-state",
      code_challenge: challenge,
      code_challenge_method: "S256",
      resource,
      scope: "messages:read messages:write messages:delete",
    }).toString();
  const screen = await fetchNoRedirect(authorizeURL);
  if (screen.status !== 200) {
    throw new Error(`the authorization screen answered ${screen.status}`);
  }
  // The context cookie is `Secure`, and this harness speaks plain HTTP to a
  // loopback listener, so it is carried by hand rather than by a cookie jar
  // that would silently drop it.
  const setCookie = screen.headers.getSetCookie().find((c) => c.startsWith("agm_oauth_context="));
  if (!setCookie) throw new Error("the authorization screen set no context cookie");
  const cookie = setCookie.split(";")[0];
  const fields = hiddenFields(await screen.text());

  step("the enrollment code, submitted");
  const submitted = await fetchNoRedirect(`${SERVER}/oauth/authorize`, {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded", Cookie: cookie },
    body:
      formBody({ ...fields, enrollment_code: code }) +
      "&scope_selected=messages%3Aread&scope_selected=messages%3Awrite&scope_selected=messages%3Adelete",
  });
  if (submitted.status !== 303) {
    throw new Error(
      `submitting the approval form answered ${submitted.status}, want 303:\n` +
        (await submitted.text()),
    );
  }
  const requestID = submitted.headers.get("location").replace("/oauth/requests/", "");

  step("owner approval");
  const approved = await fetch(
    `${SERVER}/v1/admin/authorization-requests/${requestID}/approve`,
    {
      method: "POST",
      headers: { "Content-Type": "application/json", Authorization: `Bearer ${admin}` },
      body: "{}",
    },
  );
  if (approved.status !== 200) {
    throw new Error(`approving answered ${approved.status}: ${await approved.text()}`);
  }

  step("completion");
  const completed = await fetchNoRedirect(`${SERVER}/oauth/requests/${requestID}/complete`, {
    method: "POST",
    headers: {
      "Content-Type": "application/x-www-form-urlencoded",
      Cookie: cookie,
      // Section 9.5 requires a PRESENT same-origin Origin here. A browser
      // sends one; this harness plays the browser, so it sends one too.
      Origin: SERVER,
    },
    body: formBody({ form_token: fields.form_token }),
  });
  if (completed.status !== 303) {
    throw new Error(
      `completing answered ${completed.status}, want 303:\n` + (await completed.text()),
    );
  }
  const callback = new URL(completed.headers.get("location"));
  if (callback.searchParams.get("iss") !== SERVER) {
    throw new Error("the callback's iss is not the byte-exact issuer");
  }
  const authorizationCode = callback.searchParams.get("code");
  if (!authorizationCode) throw new Error("the callback carried no code");

  step("the token endpoint");
  const tokenResponse = await fetch(`${SERVER}/oauth/token`, {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: formBody({
      grant_type: "authorization_code",
      code: authorizationCode,
      client_id: clientID,
      redirect_uri: "http://127.0.0.1/conformance-callback",
      code_verifier: verifier,
    }),
  });
  const tokens = await tokenResponse.json();
  if (!tokens.access_token) {
    throw new Error(`the token endpoint returned no access token: ${JSON.stringify(tokens)}`);
  }
  return tokens.access_token;
}

/** startProxy forwards to /mcp with the bearer added, so the conformance
 *  suite -- which has no notion of this server's authorization -- sees an
 *  ordinary MCP endpoint. */
function startProxy(token) {
  const target = new URL(SERVER);
  return new Promise((resolve) => {
    const proxy = createServer((req, res) => {
      const chunks = [];
      req.on("data", (c) => chunks.push(c));
      req.on("end", () => {
        const body = Buffer.concat(chunks);
        const headers = { ...req.headers, authorization: `Bearer ${token}` };
        delete headers.host;
        if (body.length > 0) headers["content-length"] = String(body.length);
        const upstream = httpRequest(
          {
            hostname: target.hostname,
            port: target.port,
            path: "/mcp",
            method: req.method,
            headers,
          },
          (up) => {
            res.writeHead(up.statusCode, up.headers);
            up.pipe(res);
          },
        );
        upstream.on("error", (err) => {
          res.writeHead(502, { "content-type": "text/plain" });
          res.end(String(err));
        });
        if (body.length > 0) upstream.write(body);
        upstream.end();
      });
    });
    proxy.listen(0, "127.0.0.1", () => resolve(proxy));
  });
}

/** parseBaseline reads the expected-failure list. The format is the
 *  conformance package's own: a YAML sequence of scenario names, optionally
 *  with a `reason`. It is parsed by hand rather than with a YAML dependency,
 *  because adding one to run a lint is a dependency to audit for ever. */
function parseBaseline(path) {
  const out = new Set();
  for (const raw of readFileSync(path, "utf8").split("\n")) {
    const line = raw.trim();
    if (line === "" || line.startsWith("#")) continue;
    const m = /^-\s*(?:name:\s*)?["']?([A-Za-z0-9._/-]+)["']?/.exec(line);
    if (m) out.add(m[1]);
  }
  return out;
}

function runConformance(url) {
  return new Promise((resolve) => {
    const child = spawn(
      "npx",
      [
        "--yes",
        PACKAGE,
        "server",
        "--url",
        url,
        "--suite",
        SUITE,
        // A single scenario, for diagnosis. CI never sets it, and the
        // baseline check below still runs -- so a narrowed run cannot be
        // mistaken for a passing full one.
        ...(process.env.AGENT_GM_CONFORMANCE_SCENARIO
          ? ["--scenario", process.env.AGENT_GM_CONFORMANCE_SCENARIO]
          : []),
        // The spec revision to filter scenarios by. Section 8.4 requires a
        // revision that runs ZERO scenarios to be a FAILURE rather than a
        // green line that tested nothing, and that clause is only testable if
        // the revision can be chosen; so it can be. CI leaves it unset and
        // runs whatever the pinned package's suite holds.
        ...(process.env.AGENT_GM_CONFORMANCE_SPEC_VERSION
          ? ["--spec-version", process.env.AGENT_GM_CONFORMANCE_SPEC_VERSION]
          : []),
        ...(process.env.AGENT_GM_CONFORMANCE_OUTPUT
          ? ["-o", process.env.AGENT_GM_CONFORMANCE_OUTPUT]
          : []),
      ],
      { stdio: ["ignore", "pipe", "inherit"] },
    );
    let stdout = "";
    child.stdout.on("data", (c) => {
      stdout += c;
      process.stderr.write(c);
    });
    child.on("close", (code) => resolve({ code, stdout }));
  });
}

/** extractResults reads the suite's own SUMMARY block.
 *
 * The block is parsed rather than the --verbose JSON because the JSON's shape
 * has changed between releases and the summary has not; and because the
 * summary is what a person reads in CI, so a test that agrees with it cannot
 * disagree with what the log shows. A run whose summary cannot be read is a
 * FAILURE here, not a pass: a run whose outcome cannot be read has measured
 * nothing.
 */
function extractResults(stdout) {
  const marker = stdout.indexOf("=== SUMMARY ===");
  if (marker < 0) return null;
  const out = [];
  for (const raw of stdout.slice(marker).split("\n")) {
    const line = raw.trim();
    const m = /^[\u2713\u2717]\s+([A-Za-z0-9._/-]+):\s+(\d+)\s+passed,\s+(\d+)\s+failed$/.exec(line);
    if (!m) continue;
    out.push({ name: m[1], passed: Number(m[3]) === 0 });
  }
  return out;
}

async function main() {
  const token = await mintTokenThroughTheWholeFlow();
  const proxy = await startProxy(token);
  const url = `http://127.0.0.1:${proxy.address().port}/`;
  step(`running ${PACKAGE} against ${url}`);

  const { code, stdout } = await runConformance(url);
  proxy.close();

  if (process.env.AGENT_GM_CONFORMANCE_SCENARIO) {
    process.stderr.write(
      "conformance: a single scenario was run for diagnosis, so the baseline was " +
        "NOT checked. This mode is never used in CI.\n",
    );
    process.exit(code);
  }

  const results = extractResults(stdout);
  if (results === null) {
    die(
      "conformance: the suite's output could not be read as results. That is a " +
        "failure, not a pass: a run whose outcome cannot be read has measured nothing.",
    );
  }
  if (results.length === 0) {
    die(
      "conformance: the chosen spec revision ran ZERO scenarios. That is a " +
        "failure, not a green line that tested nothing (section 8.4).",
    );
  }

  const baseline = parseBaseline(BASELINE);
  const newFailures = results.filter((r) => !r.passed && !baseline.has(r.name));
  const fixed = results.filter((r) => r.passed && baseline.has(r.name));

  process.stderr.write(`conformance: ${results.length} scenarios\n`);
  for (const r of newFailures) {
    process.stderr.write(`conformance: NEW FAILURE ${r.name}\n`);
  }
  for (const r of fixed) {
    process.stderr.write(
      `conformance: ${r.name} is in the baseline and now PASSES; remove it. ` +
        "The file is checked in both directions so it cannot rot into a list of excuses.\n",
    );
  }
  if (newFailures.length > 0 || fixed.length > 0) {
    process.exit(1);
  }
  process.stderr.write("conformance: the baseline holds in both directions\n");
  process.exit(code === 0 || baseline.size > 0 ? 0 : code);
}

main().catch((err) => die(`conformance: ${err.message}`));
