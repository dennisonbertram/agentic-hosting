import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import { Readable } from 'node:stream';
import { afterEach, test } from 'node:test';
import { createPinnedLookup, createRelayServer, resolvePublicIPv4 } from './relay.mjs';

const API_KEY = 'synthetic-upstream-key-never-log';
const RELAY_TOKEN = 'synthetic-relay-token-0123456789abcdef';
const servers = new Set();

const basePayload = () => ({
  model: 'anthropic/claude-sonnet-5.5',
  messages: [
    { role: 'system', content: 'You are a bounded calculator assistant.' },
    { role: 'user', content: 'Add 19 and 23.' },
  ],
  tools: [{
    type: 'function',
    function: {
      name: 'add_numbers',
      description: 'Add two numbers.',
      parameters: {
        type: 'object',
        properties: { a: { type: 'number' }, b: { type: 'number' } },
        required: ['a', 'b'],
        additionalProperties: false,
      },
    },
  }],
  tool_choice: { type: 'function', function: { name: 'add_numbers' } },
  parallel_tool_calls: false,
  max_tokens: 512,
  reasoning: { effort: 'low' },
  stream: false,
});

async function withServer(options, callback) {
  const server = createRelayServer({
    apiKey: API_KEY,
    authToken: RELAY_TOKEN,
    resolve4Impl: async () => ['8.8.8.8'],
    providerRequest: async () => ({
      statusCode: 200,
      body: JSON.stringify({ choices: [{ message: { content: 'ok' } }] }),
    }),
    ...options,
  });
  servers.add(server);
  await new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', resolve);
  });
  const address = server.address();
  try {
    return await callback(`http://127.0.0.1:${address.port}`);
  } finally {
    await new Promise((resolve, reject) => {
      server.close((error) => (error ? reject(error) : resolve()));
    });
    servers.delete(server);
  }
}

afterEach(async () => {
  await Promise.all([...servers].map((server) => new Promise((resolve) => server.close(resolve))));
  servers.clear();
});

test('requires relay bearer auth and only accepts the fixed chat-completions path', async () => {
  let providerCalls = 0;
  await withServer({ providerRequest: async () => { providerCalls += 1; } }, async (baseURL) => {
    const missing = await fetch(`${baseURL}/api/v1/chat/completions`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(basePayload()),
    });
    assert.equal(missing.status, 401);
    assert.doesNotMatch(await missing.text(), /synthetic-relay-token|synthetic-upstream-key/);

    const wrongPath = await fetch(`${baseURL}/api/v1/chat/completions?url=http://127.0.0.1`, {
      method: 'POST',
      headers: { Authorization: `Bearer ${RELAY_TOKEN}`, 'Content-Type': 'application/json' },
      body: JSON.stringify(basePayload()),
    });
    assert.equal(wrongPath.status, 404);
    assert.equal(providerCalls, 0);
  });
});

test('pins destination, model, headers, tools, and output budget before provider call', async () => {
  let call;
  await withServer({
    providerRequest: async (request) => {
      call = request;
      return { statusCode: 200, body: JSON.stringify({ choices: [{ message: { content: 'ok' } }] }) };
    },
  }, async (baseURL) => {
    const payload = basePayload();
    payload.max_tokens = 999_999;
    const response = await fetch(`${baseURL}/api/v1/chat/completions`, {
      method: 'POST',
      headers: {
        Authorization: `Bearer ${RELAY_TOKEN}`,
        'Content-Type': 'application/json',
        'X-Forwarded-Host': 'attacker.example',
      },
      body: JSON.stringify(payload),
    });
    assert.equal(response.status, 200);
    assert.deepEqual(await response.json(), { choices: [{ message: { content: 'ok' } }] });
  });

  assert.equal(call.url, 'https://openrouter.ai/api/v1/chat/completions');
  assert.equal(call.apiKey, API_KEY);
  assert.equal(call.headers.Authorization, `Bearer ${API_KEY}`);
  assert.equal(call.headers['X-Forwarded-Host'], undefined);
  assert.equal(call.payload.model, 'anthropic/claude-sonnet-5.5');
  assert.equal(call.payload.max_tokens, 2048);
  assert.equal(call.payload.stream, false);
  assert.equal(call.payload.tools.length, 1);
  assert.equal(call.payload.tools[0].function.name, 'add_numbers');
  assert.doesNotMatch(JSON.stringify(call), /synthetic-relay-token/);
});

test('preserves bounded assistant reasoning details for the second tool round', async () => {
  let sentMessages;
  const reasoningDetails = [{ type: 'reasoning.text', text: 'synthetic opaque reasoning detail' }];
  await withServer({
    providerRequest: async ({ payload }) => {
      sentMessages = payload.messages;
      return { statusCode: 200, body: JSON.stringify({ choices: [{ message: { content: '42' } }] }) };
    },
  }, async (baseURL) => {
    const payload = basePayload();
    payload.tool_choice = 'none';
    payload.messages.push(
      {
        role: 'assistant',
        content: null,
        reasoning_details: reasoningDetails,
        tool_calls: [{
          id: 'call_1',
          type: 'function',
          function: { name: 'add_numbers', arguments: '{"a":19,"b":23}' },
        }],
      },
      { role: 'tool', tool_call_id: 'call_1', content: '42' },
    );
    const response = await fetch(`${baseURL}/api/v1/chat/completions`, {
      method: 'POST',
      headers: { Authorization: `Bearer ${RELAY_TOKEN}`, 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
    assert.equal(response.status, 200);
  });
  assert.deepEqual(sentMessages[2].reasoning_details, reasoningDetails);
});

test('rejects caller-selected models, destinations, tools, and malformed bounded messages', async () => {
  let providerCalls = 0;
  const cases = [
    (body) => { body.model = 'other/model'; },
    (body) => { body.url = 'http://127.0.0.1:8080/admin'; },
    (body) => { body.tools[0].function.name = 'run_shell'; },
    (body) => { body.messages[1].content = 'x'.repeat(1001); },
    (body) => { body.stream = true; },
  ];
  await withServer({ providerRequest: async () => { providerCalls += 1; } }, async (baseURL) => {
    for (const mutate of cases) {
      const payload = basePayload();
      mutate(payload);
      const response = await fetch(`${baseURL}/api/v1/chat/completions`, {
        method: 'POST',
        headers: { Authorization: `Bearer ${RELAY_TOKEN}`, 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
      });
      assert.equal(response.status, 400);
    }
    const oversized = await fetch(`${baseURL}/api/v1/chat/completions`, {
      method: 'POST',
      headers: { Authorization: `Bearer ${RELAY_TOKEN}`, 'Content-Type': 'application/json' },
      body: JSON.stringify({ message: 'x'.repeat(9000) }),
    });
    assert.equal(oversized.status, 413);
  });
  assert.equal(providerCalls, 0);
});

test('fails closed for private DNS answers and never calls transport', async () => {
  await assert.rejects(
    resolvePublicIPv4(async () => ['127.0.0.1']),
    /provider DNS did not resolve to public IPv4 addresses/,
  );
  await assert.rejects(
    resolvePublicIPv4(async () => ['8.8.8.8', '10.0.0.2']),
    /provider DNS did not resolve to public IPv4 addresses/,
  );
});

test('pinned HTTPS lookup supports Node all-address callbacks and keeps the validated host', async () => {
  const lookup = createPinnedLookup('8.8.8.8');
  lookup('openrouter.ai', { all: true }, (error, addresses) => {
    assert.equal(error, null);
    assert.deepEqual(addresses, [{ address: '8.8.8.8', family: 4 }]);
  });
  lookup('openrouter.ai', {}, (error, address, family) => {
    assert.equal(error, null);
    assert.equal(address, '8.8.8.8');
    assert.equal(family, 4);
  });

  let seenOptions;
  await withServer({
    providerRequest: undefined,
    resolve4Impl: async () => ['8.8.8.8'],
    httpsRequestImpl: (options, onResponse) => {
      seenOptions = options;
      const request = new EventEmitter();
      request.setTimeout = () => {};
      request.end = () => {
        options.lookup(options.hostname, { all: true }, (error, addresses) => {
          assert.equal(error, null);
          assert.deepEqual(addresses, [{ address: '8.8.8.8', family: 4 }]);
          const response = Readable.from([Buffer.from(JSON.stringify({ choices: [{ message: { content: 'ok' } }] }))]);
          response.statusCode = 200;
          onResponse(response);
        });
      };
      return request;
    },
  }, async (baseURL) => {
    const response = await fetch(`${baseURL}/api/v1/chat/completions`, {
      method: 'POST',
      headers: { Authorization: `Bearer ${RELAY_TOKEN}`, 'Content-Type': 'application/json' },
      body: JSON.stringify(basePayload()),
    });
    assert.equal(response.status, 200);
  });
  assert.equal(seenOptions.hostname, 'openrouter.ai');
  assert.equal(seenOptions.servername, 'openrouter.ai');
  assert.equal(seenOptions.port, 443);
  assert.equal(seenOptions.path, '/api/v1/chat/completions');
  assert.equal(seenOptions.rejectUnauthorized, true);
});

test('bounds DNS resolution within the same total provider deadline', async () => {
  await withServer({
    providerRequest: undefined,
    providerTimeoutMs: 25,
    resolve4Impl: () => new Promise(() => {}),
    httpsRequestImpl: () => { throw new Error('HTTPS must not start after DNS timeout'); },
  }, async (baseURL) => {
    const response = await fetch(`${baseURL}/api/v1/chat/completions`, {
      method: 'POST',
      headers: { Authorization: `Bearer ${RELAY_TOKEN}`, 'Content-Type': 'application/json' },
      body: JSON.stringify(basePayload()),
    });
    assert.equal(response.status, 502);
    assert.deepEqual(await response.json(), { error: 'provider request failed' });
  });
});

test('hides provider errors and caps provider response size', async () => {
  const providerResponses = [
    { statusCode: 503, body: 'synthetic private provider error synthetic-upstream-key-never-log' },
    { statusCode: 200, body: `{"choices":[]}${' '.repeat(128 * 1024)}` },
  ];
  await withServer({
    providerRequest: async () => providerResponses.shift(),
  }, async (baseURL) => {
    for (let i = 0; i < 2; i += 1) {
      const response = await fetch(`${baseURL}/api/v1/chat/completions`, {
        method: 'POST',
        headers: { Authorization: `Bearer ${RELAY_TOKEN}`, 'Content-Type': 'application/json' },
        body: JSON.stringify(basePayload()),
      });
      assert.equal(response.status, 502);
      assert.doesNotMatch(await response.text(), /synthetic private provider error|synthetic-upstream-key/);
    }
  });
});

test('caps lifetime requests and permits only one active provider request', async () => {
  let providerCalls = 0;
  let unblock;
  let markEntered;
  const entered = new Promise((resolve) => { markEntered = resolve; });
  const blocked = new Promise((resolve) => { unblock = resolve; });
  await withServer({
    providerRequest: async () => {
      providerCalls += 1;
      if (providerCalls === 1) {
        markEntered();
        await blocked;
      }
      return { statusCode: 200, body: JSON.stringify({ choices: [] }) };
    },
  }, async (baseURL) => {
    const request = () => fetch(`${baseURL}/api/v1/chat/completions`, {
      method: 'POST',
      headers: { Authorization: `Bearer ${RELAY_TOKEN}`, 'Content-Type': 'application/json' },
      body: JSON.stringify(basePayload()),
    });
    const first = request();
    await entered;
    const concurrent = await request();
    assert.equal(concurrent.status, 429);
    unblock();
    assert.equal((await first).status, 200);
    for (let i = 1; i < 8; i += 1) assert.equal((await request()).status, 200);
    assert.equal((await request()).status, 429);
  });
  assert.equal(providerCalls, 8);
});
