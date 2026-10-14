import assert from 'node:assert/strict';
import { afterEach, test } from 'node:test';
import { createAgentServer } from './app.mjs';

const API_KEY = 'test-openrouter-key-do-not-log';
const AUTH_TOKEN = 'test-demo-token-0123456789abcdef';
const servers = new Set();

async function withServer(options, callback) {
  const server = createAgentServer({
    apiKey: API_KEY,
    authToken: AUTH_TOKEN,
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

test('requires the demo bearer token and does not call the provider when missing', async () => {
  let providerCalls = 0;
  await withServer({ fetchImpl: async () => { providerCalls += 1; } }, async (baseURL) => {
    const response = await fetch(`${baseURL}/chat`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ message: 'Add 19 and 23.' }),
    });
    assert.equal(response.status, 401);
    assert.equal(providerCalls, 0);
    assert.doesNotMatch(await response.text(), /test-demo-token|test-openrouter-key/);
  });
});

test('rejects oversized input before calling the provider', async () => {
  let providerCalls = 0;
  await withServer({ fetchImpl: async () => { providerCalls += 1; } }, async (baseURL) => {
    const response = await fetch(`${baseURL}/chat`, {
      method: 'POST',
      headers: {
        Authorization: `Bearer ${AUTH_TOKEN}`,
        'Content-Type': 'application/json',
      },
      body: JSON.stringify({ message: 'x'.repeat(5000) }),
    });
    assert.equal(response.status, 413);
    assert.equal(providerCalls, 0);
  });
});

test('runs one bounded calculator tool round-trip with the fixed model and endpoint', async () => {
  const calls = [];
  const fetchImpl = async (url, init) => {
    calls.push({ url, init, body: JSON.parse(init.body) });
    if (calls.length === 1) {
      return new Response(JSON.stringify({
        choices: [{
          finish_reason: 'tool_calls',
          message: {
            role: 'assistant',
            content: null,
            tool_calls: [{
              id: 'call_test_1',
              type: 'function',
              function: { name: 'add_numbers', arguments: '{"a":19,"b":23}' },
            }],
          },
        }],
      }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    }
    return new Response(JSON.stringify({
      choices: [{
        finish_reason: 'stop',
        message: { role: 'assistant', content: '19 plus 23 is 42.' },
      }],
    }), { status: 200, headers: { 'Content-Type': 'application/json' } });
  };

  await withServer({ fetchImpl }, async (baseURL) => {
    const response = await fetch(`${baseURL}/chat`, {
      method: 'POST',
      headers: {
        Authorization: `Bearer ${AUTH_TOKEN}`,
        'Content-Type': 'application/json',
      },
      body: JSON.stringify({ message: 'Add 19 and 23, then explain the result.' }),
    });
    assert.equal(response.status, 200);
    assert.deepEqual(await response.json(), {
      answer: '19 plus 23 is 42.',
      tool: { name: 'add_numbers', result: 42 },
    });
  });

  assert.equal(calls.length, 2);
  assert.ok(calls.every(({ url }) => url === 'https://openrouter.ai/api/v1/chat/completions'));
  assert.ok(calls.every(({ init }) => init.headers.Authorization === `Bearer ${API_KEY}`));
  assert.ok(calls.every(({ body }) => body.model === 'anthropic/claude-sonnet-5.5'));
  assert.ok(calls.every(({ body }) => body.max_tokens <= 512));
  assert.ok(calls.every(({ body }) => body.reasoning.effort === 'low'));
  assert.ok(calls.every(({ body }) => body.tools[0].function.strict === true));
  assert.equal(calls[0].body.tool_choice, 'auto');
  assert.equal(calls[1].body.tool_choice, 'none');
  assert.equal(calls[1].body.messages.at(-1).tool_call_id, 'call_test_1');
  assert.equal(calls[1].body.messages.at(-1).content, '42');
  assert.doesNotMatch(JSON.stringify(calls), /test-demo-token/);
});

test('uses only the fixed authenticated relay endpoint when relay mode is selected', async () => {
  const relayToken = 'test-relay-token-0123456789abcdef';
  const calls = [];
  const fetchImpl = async (url, init) => {
    calls.push({ url, init, body: JSON.parse(init.body) });
    const completion = calls.length === 1
      ? {
          finish_reason: 'tool_calls',
          message: {
            role: 'assistant',
            content: null,
            tool_calls: [{
              id: 'relay_call_1',
              type: 'function',
              function: { name: 'add_numbers', arguments: '{"a":4,"b":5}' },
            }],
          },
        }
      : { finish_reason: 'stop', message: { role: 'assistant', content: '4 plus 5 is 9.' } };
    return new Response(JSON.stringify({ choices: [completion] }), {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    });
  };

  await withServer({ apiKey: undefined, relayToken, fetchImpl }, async (baseURL) => {
    const response = await fetch(`${baseURL}/chat`, {
      method: 'POST',
      headers: {
        Authorization: `Bearer ${AUTH_TOKEN}`,
        'Content-Type': 'application/json',
      },
      body: JSON.stringify({ message: 'Add 4 and 5.' }),
    });
    assert.equal(response.status, 200);
    assert.deepEqual(await response.json(), {
      answer: '4 plus 5 is 9.',
      tool: { name: 'add_numbers', result: 9 },
    });
  });

  assert.equal(calls.length, 2);
  assert.ok(calls.every(({ url }) => url === 'http://openrouter-demo-relay:8080/api/v1/chat/completions'));
  assert.ok(calls.every(({ init }) => init.headers.Authorization === `Bearer ${relayToken}`));
  assert.ok(calls.every(({ body }) => body.model === 'anthropic/claude-sonnet-5.5'));
  assert.ok(calls.every(({ body }) => body.max_tokens <= 512));
  assert.ok(calls.every(({ body }) => body.tools[0].function.strict === true));
  assert.equal(calls[0].body.tool_choice, 'auto');
  assert.equal(calls[1].body.tool_choice, 'none');
  assert.doesNotMatch(JSON.stringify(calls), /test-openrouter-key|https:\/\/openrouter\.ai\/api/);
});

test('allows only a validated private relay IP override with the fixed endpoint and token', async () => {
  const relayToken = 'test-relay-token-0123456789abcdef';
  const calls = [];
  const fetchImpl = async (url, init) => {
    calls.push({ url, init });
    return new Response('{}', { status: 502 });
  };

  await withServer({ apiKey: undefined, relayToken, relayIP: '172.20.7.3', fetchImpl }, async (baseURL) => {
    const response = await fetch(`${baseURL}/chat`, {
      method: 'POST',
      headers: {
        Authorization: `Bearer ${AUTH_TOKEN}`,
        'Content-Type': 'application/json',
      },
      body: JSON.stringify({ message: 'Add 4 and 5.' }),
    });
    assert.equal(response.status, 502);
  });

  assert.equal(calls.length, 1);
  assert.equal(calls[0].url, 'http://172.20.7.3:8080/api/v1/chat/completions');
  assert.equal(calls[0].init.headers.Authorization, `Bearer ${relayToken}`);
  assert.doesNotMatch(calls[0].url, /openrouter\.ai|https:/);
});

test('rejects unsafe relay IP overrides and an override without relay mode', () => {
  const auth = { authToken: AUTH_TOKEN };
  const relay = { relayToken: 'test-relay-token-0123456789abcdef' };
  for (const relayIP of [
    '8.8.8.8',
    '127.0.0.1',
    '172.20.7.0',
    '172.20.7.1',
    '172.20.7.255',
    '172.24.0.2',
    '172.20.7.3:8080',
    'http://172.20.7.3',
    '172.20.7.03',
    '::1',
  ]) {
    assert.throws(() => createAgentServer({ ...auth, ...relay, relayIP }), /PROVIDER_RELAY_IP/);
  }
  assert.throws(() => createAgentServer({
    ...auth,
    apiKey: API_KEY,
    relayIP: '172.20.7.3',
  }), /PROVIDER_RELAY_IP requires PROVIDER_RELAY_TOKEN/);
});

test('rejects configurations that expose a direct key in relay mode', () => {
  assert.throws(() => createAgentServer({
    apiKey: API_KEY,
    relayToken: 'test-relay-token-0123456789abcdef',
    authToken: AUTH_TOKEN,
  }), /OPENROUTER_API_KEY must not be set in provider relay mode/);
});

test('filters assistant metadata for the relay continuation while preserving reasoning details', async () => {
  const reasoningDetails = [{ type: 'reasoning.text', text: 'opaque provider detail' }];
  let continuationAssistant;
  let providerCalls = 0;
  const fetchImpl = async (_url, init) => {
    providerCalls += 1;
    const messages = JSON.parse(init.body).messages;
    if (providerCalls === 2) continuationAssistant = messages.at(-2);
    const choice = providerCalls === 1
      ? {
          finish_reason: 'tool_calls',
          message: {
            role: 'assistant',
            content: null,
            reasoning: 'additional provider metadata',
            refusal: null,
            annotations: [],
            reasoning_details: reasoningDetails,
            tool_calls: [{
              id: 'metadata_call_1',
              type: 'function',
              index: 0,
              function: {
                name: 'add_numbers',
                arguments: '{"a":8,"b":13}',
                provider_metadata: 'discard this',
              },
            }],
          },
        }
      : { finish_reason: 'stop', message: { role: 'assistant', content: 'The answer is 21.' } };
    return new Response(JSON.stringify({ choices: [choice] }), {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    });
  };

  await withServer({ apiKey: undefined, relayToken: 'test-relay-token-0123456789abcdef', fetchImpl }, async (baseURL) => {
    const response = await fetch(`${baseURL}/chat`, {
      method: 'POST',
      headers: {
        Authorization: `Bearer ${AUTH_TOKEN}`,
        'Content-Type': 'application/json',
      },
      body: JSON.stringify({ message: 'Add 8 and 13.' }),
    });
    assert.equal(response.status, 200);
  });

  assert.deepEqual(Object.keys(continuationAssistant).sort(), [
    'content', 'reasoning_details', 'role', 'tool_calls',
  ]);
  assert.deepEqual(continuationAssistant.reasoning_details, reasoningDetails);
  assert.deepEqual(continuationAssistant.tool_calls, [{
    id: 'metadata_call_1',
    type: 'function',
    function: { name: 'add_numbers', arguments: '{"a":8,"b":13}' },
  }]);
  assert.equal(continuationAssistant.reasoning, undefined);
  assert.equal(continuationAssistant.refusal, undefined);
  assert.equal(continuationAssistant.annotations, undefined);
});

test('does not execute or loop on unsupported tool calls and hides provider errors', async () => {
  let providerCalls = 0;
  const fetchImpl = async () => {
    providerCalls += 1;
    return new Response(JSON.stringify({
      choices: [{
        finish_reason: 'tool_calls',
        message: {
          role: 'assistant',
          content: null,
          tool_calls: [{
            id: 'call_untrusted',
            type: 'function',
            function: { name: 'run_shell', arguments: '{"command":"id"}' },
          }],
        },
      }],
      error: { message: 'provider payload must not be echoed' },
    }), { status: 200, headers: { 'Content-Type': 'application/json' } });
  };

  await withServer({ fetchImpl }, async (baseURL) => {
    const response = await fetch(`${baseURL}/chat`, {
      method: 'POST',
      headers: {
        Authorization: `Bearer ${AUTH_TOKEN}`,
        'Content-Type': 'application/json',
      },
      body: JSON.stringify({ message: 'Please do an unsafe operation.' }),
    });
    assert.equal(response.status, 502);
    const text = await response.text();
    assert.doesNotMatch(text, /provider payload|test-openrouter-key|test-demo-token/);
  });

  assert.equal(providerCalls, 1);
});

test('logs only a sanitized provider status and constant code on provider failure', async () => {
  const sentinels = {
    key: 'OPENROUTER_KEY_SENTINEL_SECRET',
    token: 'DEMO_TOKEN_SENTINEL_SECRET_0123456789',
    prompt: 'PROMPT_SENTINEL_PRIVATE_INPUT',
    providerBody: 'PROVIDER_BODY_SENTINEL_PRIVATE_ERROR',
  };
  const diagnostics = [];
  await withServer({
    apiKey: sentinels.key,
    authToken: sentinels.token,
    diagnosticSink: (record) => diagnostics.push(record),
    fetchImpl: async () => new Response(JSON.stringify({ error: { message: sentinels.providerBody } }), {
      status: 429,
      headers: { 'Content-Type': 'application/json' },
    }),
  }, async (baseURL) => {
    const response = await fetch(`${baseURL}/chat`, {
      method: 'POST',
      headers: {
        Authorization: `Bearer ${sentinels.token}`,
        'Content-Type': 'application/json',
      },
      body: JSON.stringify({ message: sentinels.prompt }),
    });
    assert.equal(response.status, 502);
    assert.deepEqual(await response.json(), { error: 'agent request failed' });
  });

  assert.deepEqual(diagnostics, [{
    event: 'openrouter_agent_failure',
    code: 'provider_http_error',
    provider_status: 429,
  }]);
  assert.doesNotMatch(JSON.stringify(diagnostics), new RegExp(Object.values(sentinels).join('|')));
});

test('diagnoses missing tool calls and truncated final answers with allowlisted reasons', async () => {
  const diagnostics = [];
  let providerCalls = 0;
  const fetchImpl = async () => {
    providerCalls += 1;
    const choice = providerCalls === 1
      ? { finish_reason: 'tool_calls', message: {
          role: 'assistant', content: null, tool_calls: [{
            id: 'diagnostic_call_1',
            type: 'function',
            function: { name: 'add_numbers', arguments: '{"a":2,"b":3}' },
          }],
        } }
      : { finish_reason: 'length', message: { role: 'assistant', content: 'The answer may be' } };
    return new Response(JSON.stringify({ choices: [choice] }), {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    });
  };

  await withServer({ diagnosticSink: (record) => diagnostics.push(record), fetchImpl }, async (baseURL) => {
    const response = await fetch(`${baseURL}/chat`, {
      method: 'POST',
      headers: {
        Authorization: `Bearer ${AUTH_TOKEN}`,
        'Content-Type': 'application/json',
      },
      body: JSON.stringify({ message: 'Add 2 and 3.' }),
    });
    assert.equal(response.status, 502);
  });
  assert.deepEqual(diagnostics, [{
    event: 'openrouter_agent_failure',
    code: 'provider_final_answer_truncated',
    provider_finish_reason: 'length',
  }]);

  diagnostics.length = 0;
  await withServer({
    diagnosticSink: (record) => diagnostics.push(record),
    fetchImpl: async () => new Response(JSON.stringify({ choices: [{
      finish_reason: 'stop',
      message: { role: 'assistant', content: 'The sum is 5.' },
    }] }), { status: 200, headers: { 'Content-Type': 'application/json' } }),
  }, async (baseURL) => {
    const response = await fetch(`${baseURL}/chat`, {
      method: 'POST',
      headers: {
        Authorization: `Bearer ${AUTH_TOKEN}`,
        'Content-Type': 'application/json',
      },
      body: JSON.stringify({ message: 'Add 2 and 3.' }),
    });
    assert.equal(response.status, 502);
  });
  assert.deepEqual(diagnostics, [{
    event: 'openrouter_agent_failure',
    code: 'provider_tool_call_missing',
    provider_finish_reason: 'stop',
  }]);
});

test('stops after the final-answer request if the provider tries another tool call', async () => {
  let providerCalls = 0;
  const fetchImpl = async () => {
    providerCalls += 1;
    return new Response(JSON.stringify({
      choices: [{
        finish_reason: 'tool_calls',
        message: {
          role: 'assistant',
          content: null,
          tool_calls: [{
            id: `call_${providerCalls}`,
            type: 'function',
            function: { name: 'add_numbers', arguments: '{"a":2,"b":3}' },
          }],
        },
      }],
    }), { status: 200, headers: { 'Content-Type': 'application/json' } });
  };

  await withServer({ fetchImpl }, async (baseURL) => {
    const response = await fetch(`${baseURL}/chat`, {
      method: 'POST',
      headers: {
        Authorization: `Bearer ${AUTH_TOKEN}`,
        'Content-Type': 'application/json',
      },
      body: JSON.stringify({ message: 'Add 2 and 3.' }),
    });
    assert.equal(response.status, 502);
    assert.deepEqual(await response.json(), { error: 'agent request failed' });
  });

  assert.equal(providerCalls, 2);
});

test('bounds provider wait and returns a generic error on timeout', async () => {
  let providerCalls = 0;
  const diagnostics = [];
  const fetchImpl = async (_url, init) => {
    providerCalls += 1;
    await new Promise((_resolve, reject) => {
      init.signal.addEventListener('abort', () => reject(init.signal.reason), { once: true });
    });
  };

  await withServer({
    fetchImpl,
    providerTimeoutMs: 20,
    diagnosticSink: (record) => diagnostics.push(record),
  }, async (baseURL) => {
    const response = await fetch(`${baseURL}/chat`, {
      method: 'POST',
      headers: {
        Authorization: `Bearer ${AUTH_TOKEN}`,
        'Content-Type': 'application/json',
      },
      body: JSON.stringify({ message: 'Add 2 and 3.' }),
    });
    assert.equal(response.status, 502);
    assert.deepEqual(await response.json(), { error: 'agent request failed' });
  });

  assert.equal(providerCalls, 1);
  assert.deepEqual(diagnostics, [{ event: 'openrouter_agent_failure', code: 'provider_timeout' }]);
});

test('exposes a small public health response without contacting the provider', async () => {
  let providerCalls = 0;
  await withServer({ fetchImpl: async () => { providerCalls += 1; } }, async (baseURL) => {
    const response = await fetch(`${baseURL}/health`);
    assert.equal(response.status, 200);
    assert.deepEqual(await response.json(), { status: 'ok' });
    assert.equal(response.headers.get('cache-control'), 'no-store');
  });
  assert.equal(providerCalls, 0);
});
