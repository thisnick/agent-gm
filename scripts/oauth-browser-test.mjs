// Run with `devbox run test-oauth-browser`. Uses a disposable server, CLI
// credential file and dynamically registered OAuth client; never a real account.
import assert from 'node:assert/strict';
import { execFile, spawn } from 'node:child_process';
import { createHash, randomBytes } from 'node:crypto';
import { mkdtemp, rm } from 'node:fs/promises';
import { createServer } from 'node:http';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { promisify } from 'node:util';
const exec = promisify(execFile);
const dir = await mkdtemp(join(tmpdir(), 'agm-browser-'));
const session = `agm-test-${process.pid}`;
const browser = async (...args) => (await exec('npx', ['--yes', 'agent-browser@0.27.0', '--session', session, ...args], {maxBuffer: 4 * 1024 * 1024})).stdout;
const listen = server => new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
const secret = randomBytes(32).toString('hex');
let server;
let expected;
let callbackResult;
let callbackError;
let base;
const callback = createServer(async (req, res) => {
  if (!req.url.startsWith('/callback?')) { res.writeHead(404).end(); return; }
  try {
    const q = new URL(req.url, 'http://localhost').searchParams;
    assert.equal(q.get('state'), expected.state);
    assert.equal(q.get('iss'), base);
    if (expected.denied) {
      assert.equal(q.get('error'), 'access_denied');
      assert.equal(q.get('code'), null);
      callbackResult = 'Denial returned correctly';
    } else {
      const response = await fetch(base + '/oauth/token', {method: 'POST', body: new URLSearchParams({
        grant_type: 'authorization_code', code: q.get('code'), client_id: expected.client,
        redirect_uri: expected.redirect, code_verifier: expected.verifier,
      })});
      assert.equal(response.status, 200);
      const token = await response.json();
      assert.ok(token.access_token);
      assert.equal(token.scope, 'messages:read');
      callbackResult = 'OAuth complete: state, issuer, PKCE and token exchange verified';
    }
    res.writeHead(200, {'Content-Type': 'text/html'}).end(`<h1>${callbackResult}</h1>`);
  } catch (error) { callbackError = error; res.writeHead(500).end('Callback verification failed'); }
});
try {
  await exec('go', ['build', '-o', join(dir, 'server'), './cmd/agent-gm']);
  await exec('go', ['build', '-o', join(dir, 'agm'), './cmd/agm']);
  await listen(callback);
  const portProbe = createServer();
  await listen(portProbe);
  base = `http://127.0.0.1:${portProbe.address().port}`;
  await new Promise(resolve => portProbe.close(resolve));
  const env = {...process.env, AGENT_GM_CREDENTIALS_FILE: join(dir, 'credentials.json'),
    AGENT_GM_DATA_DIR: join(dir, 'data'), AGENT_GM_PUBLIC_URL: base,
    AGENT_GM_LISTEN_ADDR: new URL(base).host, AGENT_GM_ADMIN_SECRET: secret,
    AGENT_GM_DATA_KEY: randomBytes(32).toString('hex')};
  // Existing shell credentials must never select a real server for this test.
  for (const key of ['AGENT_GM_URL', 'AGENT_GM_ACCESS_TOKEN', 'AGENT_GM_ACCESS_TOKEN_FILE', 'AGENT_GM_REFRESH_TOKEN_FILE', 'AGENT_GM_CLIENT_ID']) delete env[key];
  server = spawn(join(dir, 'server'), ['serve'], {env, stdio: 'ignore'});
  let ready = false;
  for (let i = 0; i < 100; i++) {
    try { ready = (await fetch(base + '/.well-known/oauth-authorization-server')).ok; } catch {}
    if (ready) break;
    await delay(100);
  }
  assert.ok(ready, 'test server must start');
  const login = spawn(join(dir, 'agm'), ['auth', 'login', '--admin', '--server', base, '--secret-stdin'], {env, stdio: ['pipe', 'ignore', 'inherit']});
  login.stdin.end(secret);
  assert.equal(await new Promise(resolve => login.on('exit', resolve)), 0);
  const cli = async (...args) => JSON.parse((await exec(join(dir, 'agm'), [...args, '--json'], {env})).stdout).data;
  for (const denied of [false, true]) {
    callbackResult = callbackError = undefined;
    const redirect = `http://127.0.0.1:${callback.address().port}/callback`;
    const registration = await fetch(base + '/oauth/register', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({client_name: 'Browser regression client', redirect_uris: [redirect], token_endpoint_auth_method: 'none', grant_types: ['authorization_code', 'refresh_token'], response_types: ['code']})});
    assert.equal(registration.status, 201);
    const client = (await registration.json()).client_id;
    const verifier = randomBytes(48).toString('base64url');
    const state = randomBytes(24).toString('base64url');
    expected = {client, verifier, state, redirect, denied};
    const enrollment = await cli('admin', 'enrollment-codes', 'create', 'browser-test', '--scopes', 'messages:read');
    const query = new URLSearchParams({response_type: 'code', client_id: client, redirect_uri: redirect, state,
      code_challenge: createHash('sha256').update(verifier).digest('base64url'), code_challenge_method: 'S256', resource: base + '/mcp', scope: 'messages:read'});
    await browser('open', base + '/oauth/authorize?' + query);
    assert.match(await browser('snapshot'), /Get an enrollment code/);
    console.log('Enrollment page loaded');
    await browser('set', 'viewport', '390', '844');
    assert.equal((await browser('eval', 'document.documentElement.scrollWidth <= window.innerWidth')).trim(), 'true', 'mobile page must not overflow');
    await browser('fill', '#enrollment_code', enrollment.code);
    await browser('click', 'button[type=submit]');
    const requestURL = (await browser('get', 'url')).trim();
    const id = requestURL.split('/').pop();
    console.log('Enrollment submitted');
    assert.match(id, /^authreq_/);
    assert.match(await browser('snapshot'), new RegExp(`authorization-requests approve ${id}`));
    if (!denied) {
      await browser('set', 'offline', 'on');
      await browser('wait', '--fn', 'document.getElementById("agm-connection").textContent.includes("Unable to check approval")');
      await browser('set', 'offline', 'off');
    }
    await cli('admin', 'authorization-requests', denied ? 'deny' : 'approve', id);
    await browser('wait', '--text', denied ? 'Request denied' : "You're approved");
    assert.match(await browser('snapshot'), /Continue to client/);
    if (!denied) {
      // GET must recover the page without consuming the approved request.
      await browser('open', requestURL + '/complete');
      assert.match(await browser('snapshot'), /You're approved/);
    }
    await browser('click', 'button[type=submit]');
    await browser('wait', '--fn', `document.body.textContent.includes('${denied ? 'Denial returned correctly' : 'OAuth complete'}')`);
    if (callbackError) throw callbackError;
    assert.ok(callbackResult);
    console.log(callbackResult);
    await cli('admin', 'clients', 'revoke', client, '--yes');
  }
  console.log('PASS: mobile layout, enrollment CLI, polling retries, automatic approval/denial, safe completion refresh, callback and PKCE exchange');
} finally {
  await browser('close').catch(() => {});
  server?.kill();
  callback.closeAllConnections();
  await new Promise(resolve => callback.close(resolve));
  await rm(dir, {recursive: true, force: true});
}
