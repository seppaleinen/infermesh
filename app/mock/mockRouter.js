// Simple Node HTTP server that mimics InferMesh router endpoints for E2E tests.
// Serves canned JSON from app/frontend/e2e/fixtures/*.json, which mirror the
// pkg/router response types (see #183). The fixtures are the single source of
// truth: edit them, not this file, to change test data.

const http = require('http');
const fs = require('fs');
const path = require('path');

// Allow overriding the port via env (see #181). Defaults to 8081.
const port = Number(process.env.MOCK_ROUTER_PORT) || 8081;

// Optional per-request delay (ms) for the workers endpoint, read from a file
// so tests can deterministically observe the loading state. The file lives in
// the same config dir as settings.yml; absent or invalid => 0 delay.
const delayFile = path.join(
  process.env.PLAYWRIGHT_TEMP_HOME || '/tmp/infermesh-test-home',
  '.config', 'infermesh', 'delay_ms'
);

function readDelayMs() {
  try {
    const n = parseInt(fs.readFileSync(delayFile, 'utf8').trim(), 10);
    return Number.isFinite(n) && n >= 0 ? n : 0;
  } catch {
    return 0;
  }
}

const fixturesDir = path.join(__dirname, '..', 'frontend', 'e2e', 'fixtures');

function loadFixture(name) {
  try {
    return JSON.parse(fs.readFileSync(path.join(fixturesDir, name), 'utf8'));
  } catch {
    return {};
  }
}

const fixtures = {
  workers: loadFixture('workers.json'),
  popularModels: loadFixture('models.json'),
  inflightRequests: loadFixture('requests.json'),
  inferenceStats: loadFixture('inference-stats.json'),
  aliases: loadFixture('aliases.json'),
  autoRouting: loadFixture('auto-routing.json'),
  connections: loadFixture('connections.json'),
};

function sendJSON(res, data) {
  res.setHeader('Content-Type', 'application/json');
  res.end(JSON.stringify(data));
}

const server = http.createServer(async (req, res) => {
  const url = new URL(req.url, 'http://localhost');

  // The workers endpoint honors a file-based delay so a spec can deterministically
  // hold the first poll in flight and observe the loading card (no client sleeps).
  if (url.pathname === '/v1/workers') {
    const ms = readDelayMs();
    if (ms > 0) await new Promise((r) => setTimeout(r, ms));
    return sendJSON(res, fixtures.workers);
  }
  if (url.pathname === '/meta/models/popular') {
    return sendJSON(res, fixtures.popularModels);
  }
  if (url.pathname === '/meta/requests/inflight') {
    return sendJSON(res, fixtures.inflightRequests);
  }
  if (url.pathname === '/v1/inference-stats') {
    return sendJSON(res, fixtures.inferenceStats);
  }
  if (url.pathname === '/meta/aliases' && req.method === 'GET') {
    return sendJSON(res, fixtures.aliases);
  }
  if (url.pathname === '/meta/auto-routing' && req.method === 'GET') {
    return sendJSON(res, fixtures.autoRouting);
  }
  if (url.pathname === '/meta/connections/count') {
    return sendJSON(res, fixtures.connections);
  }

  res.statusCode = 404;
  sendJSON(res, { error: 'not found' });
});

server.listen(port, '127.0.0.1', () => {
  console.log(`Mock router listening on http://127.0.0.1:${port}`);
});

// Fail loudly if the port is taken (e.g. a real InferMesh worker): otherwise
// tests would silently run against the wrong backend and pass/fail for
// reasons unrelated to the app under test.
server.on('error', (err) => {
  console.error(`Mock router failed to start on 127.0.0.1:${port}: ${err.message}`);
  process.exit(1);
});