// Simple Node HTTP server that mimics InferMesh router endpoints for E2E tests.
// Serves canned JSON responses matching pkg/router types.

const http = require('http');

// Fixture data matching RouterClient expectations
const fixtures = {
  workers: {
    workers: [
      {
        id: 'w-1',
        hostname: 'alpha',
        ip: '10.0.0.2',
        port: 8081,
        status: 'available',
        version: 'v1.0.0',
        loaded_models: ['llama-3-8b'],
        last_seen: new Date().toISOString(),
        in_flight: 0,
        max_in_flight: 5,
      },
      {
        id: 'w-2',
        ip: '10.0.0.3',
        port: 8082,
        status: 'busy',
        version: 'v1.0.0',
        loaded_models: [],
        last_seen: new Date().toISOString(),
        in_flight: 3,
        max_in_flight: 5,
      },
    ],
  },
  popularModels: {
    models: [
      { model: 'llama-3-8b', worker_count: 2, loaded_worker_count: 1, call_count: 42 },
      { model: 'gpt-oss-20b', worker_count: 1, loaded_worker_count: 1, call_count: 18 },
      { model: 'phi-4-mini', worker_count: 3, loaded_worker_count: 0, call_count: 99 },
    ],
  },
  inflightRequests: {
    requests: [
      {
        request_id: 'req-1',
        model: 'llama-3-8b',
        worker_id: 'w-1',
        worker_host: 'alpha',
        kind: 'chat',
        started_at: new Date(Date.now() - 5000).toISOString(),
        status: 'inflight',
      },
      {
        request_id: 'req-2',
        model: 'gpt-oss-20b',
        worker_id: 'w-2',
        worker_host: 'beta',
        kind: 'completion',
        started_at: new Date(Date.now() - 2000).toISOString(),
        finished_at: new Date().toISOString(),
        status: 'success',
        duration_ms: 1200,
      },
    ],
  },
  inferenceStats: {
    workers: [
      {
        worker_id: 'w-1',
        model: 'llama-3-8b',
        tokens_per_second: 45.2,
        avg_ttft_ms: 342,
        avg_prompt_tokens: 128,
        avg_completion_tokens: 256,
        samples: 42,
      },
    ],
  },
  aliases: { aliases: { 'gpt-4': 'llama-3-8b' } },
  autoRouting: {
    enabled: true,
    use_four_tier: true,
    models: { simple: 'small-model', medium: 'medium-model' },
    simple_max_tokens: 150,
    medium_max_tokens: 500,
  },
};

function sendJSON(res, data) {
  res.setHeader('Content-Type', 'application/json');
  res.end(JSON.stringify(data));
}

const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://localhost');

  if (url.pathname === '/v1/workers') {
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
    return sendJSON(res, { connections: 3 });
  }

  res.statusCode = 404;
  sendJSON(res, { error: 'not found' });
});

server.listen(8081, '127.0.0.1', () => {
  console.log('Mock router listening on http://127.0.0.1:8081');
});