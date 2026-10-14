import { timingSafeEqual } from 'node:crypto';
import { createServer } from 'node:http';

export const MODEL_ID = 'anthropic/claude-sonnet-5.5';
export const OPENROUTER_CHAT_URL = 'https://openrouter.ai/api/v1/chat/completions';
export const MAX_BODY_BYTES = 4096;
export const MAX_MESSAGE_LENGTH = 1000;
export const MAX_OUTPUT_TOKENS = 512;
export const PROVIDER_TIMEOUT_MS = 25_000;

const MAX_NUMBER_ABS = 1_000_000;
const MAX_PROVIDER_BODY_BYTES = 64 * 1024;
const MAX_ANSWER_LENGTH = 4000;
const toolSpec = [{
  type: 'function',
  function: {
    name: 'add_numbers',
    description: 'Add two finite numbers, each between -1000000 and 1000000.',
    parameters: {
      type: 'object',
      properties: {
        a: { type: 'number', description: 'The first number.' },
        b: { type: 'number', description: 'The second number.' },
      },
      required: ['a', 'b'],
      additionalProperties: false,
    },
  },
}];

class RequestError extends Error {
  constructor(status, publicMessage) {
    super(publicMessage);
    this.status = status;
  }
}

function writeJson(response, status, body, headers = {}) {
  response.writeHead(status, {
    'Cache-Control': 'no-store',
    'Content-Type': 'application/json; charset=utf-8',
    'X-Content-Type-Options': 'nosniff',
    ...headers,
  });
  response.end(JSON.stringify(body));
}

function tokenMatches(authorization, expectedToken) {
  if (typeof authorization !== 'string') return false;
  const match = /^Bearer ([^ ]+)$/i.exec(authorization);
  if (!match) return false;
  const actual = Buffer.from(match[1]);
  const expected = Buffer.from(expectedToken);
  return actual.length === expected.length && timingSafeEqual(actual, expected);
}

function readJsonBody(request) {
  return new Promise((resolve, reject) => {
    const contentLength = Number(request.headers['content-length']);
    if (Number.isFinite(contentLength) && contentLength > MAX_BODY_BYTES) {
      request.resume();
      reject(new RequestError(413, 'request body too large'));
      return;
    }

    const chunks = [];
    let bytes = 0;
    let settled = false;
    request.on('data', (chunk) => {
      if (settled) return;
      bytes += chunk.length;
      if (bytes > MAX_BODY_BYTES) {
        settled = true;
        chunks.length = 0;
        request.resume();
        reject(new RequestError(413, 'request body too large'));
        return;
      }
      chunks.push(chunk);
    });
    request.once('end', () => {
      if (settled) return;
      settled = true;
      try {
        resolve(JSON.parse(Buffer.concat(chunks).toString('utf8')));
      } catch {
        reject(new RequestError(400, 'request body must be valid JSON'));
      }
    });
    request.once('error', () => {
      if (settled) return;
      settled = true;
      reject(new RequestError(400, 'could not read request body'));
    });
    request.once('aborted', () => {
      if (settled) return;
      settled = true;
      reject(new RequestError(400, 'request was interrupted'));
    });
  });
}

function validateMessage(value) {
  if (typeof value !== 'string' || value.trim().length === 0 || value.length > MAX_MESSAGE_LENGTH) {
    throw new RequestError(400, `message must be a non-empty string of at most ${MAX_MESSAGE_LENGTH} characters`);
  }
  return value.trim();
}

function validateToolCall(message) {
  const calls = message?.tool_calls;
  if (!Array.isArray(calls) || calls.length !== 1) {
    throw new Error('provider did not return exactly one tool call');
  }
  const call = calls[0];
  if (
    call?.type !== 'function' ||
    call.function?.name !== 'add_numbers' ||
    typeof call.id !== 'string' ||
    !/^[A-Za-z0-9_-]{1,128}$/.test(call.id) ||
    typeof call.function.arguments !== 'string' ||
    call.function.arguments.length > 1024
  ) {
    throw new Error('provider returned an unsupported tool call');
  }

  let args;
  try {
    args = JSON.parse(call.function.arguments);
  } catch {
    throw new Error('provider returned invalid tool arguments');
  }
  if (
    !args ||
    typeof args !== 'object' ||
    Array.isArray(args) ||
    Object.keys(args).length !== 2 ||
    !Number.isFinite(args.a) ||
    !Number.isFinite(args.b) ||
    Math.abs(args.a) > MAX_NUMBER_ABS ||
    Math.abs(args.b) > MAX_NUMBER_ABS
  ) {
    throw new Error('provider returned out-of-range tool arguments');
  }
  return { id: call.id, args, call };
}

async function requestCompletion(fetchImpl, apiKey, messages, signal, toolChoice) {
  const response = await fetchImpl(OPENROUTER_CHAT_URL, {
    method: 'POST',
    headers: {
      Authorization: `Bearer ${apiKey}`,
      'Content-Type': 'application/json',
    },
    signal,
    body: JSON.stringify({
      model: MODEL_ID,
      messages,
      tools: toolSpec,
      tool_choice: toolChoice,
      parallel_tool_calls: false,
      max_tokens: MAX_OUTPUT_TOKENS,
      reasoning: { effort: 'low' },
      stream: false,
    }),
  });

  if (!response.ok) throw new Error('provider returned an error status');
  const responseText = await response.text();
  if (Buffer.byteLength(responseText, 'utf8') > MAX_PROVIDER_BODY_BYTES) {
    throw new Error('provider response exceeded the size limit');
  }
  let data;
  try {
    data = JSON.parse(responseText);
  } catch {
    throw new Error('provider returned invalid JSON');
  }
  if (data?.error || !Array.isArray(data?.choices)) {
    throw new Error('provider returned an invalid completion');
  }
  return data.choices[0];
}

async function runCalculatorAgent(message, { apiKey, fetchImpl, providerTimeoutMs }) {
  const signal = AbortSignal.timeout(providerTimeoutMs);
  const messages = [
    {
      role: 'system',
      content: 'You are a calculator assistant. Use the add_numbers tool to add the two numbers requested by the user, then explain the result briefly. Do not claim a result before using the tool.',
    },
    { role: 'user', content: message },
  ];

  const firstChoice = await requestCompletion(fetchImpl, apiKey, messages, signal, {
    type: 'function',
    function: { name: 'add_numbers' },
  });
  if (firstChoice?.finish_reason !== 'tool_calls' || !firstChoice.message) {
    throw new Error('provider did not request the required tool');
  }
  const { id, args, call } = validateToolCall(firstChoice.message);
  const result = args.a + args.b;
  if (!Number.isFinite(result)) throw new Error('tool result is out of range');

  const secondChoice = await requestCompletion(fetchImpl, apiKey, [
    ...messages,
    firstChoice.message,
    { role: 'tool', tool_call_id: id, content: String(result) },
  ], signal, 'none');
  if (
    !secondChoice?.message ||
    secondChoice.finish_reason === 'tool_calls' ||
    secondChoice.message.tool_calls?.length ||
    typeof secondChoice.message.content !== 'string' ||
    secondChoice.message.content.trim().length === 0 ||
    secondChoice.message.content.length > MAX_ANSWER_LENGTH
  ) {
    throw new Error('provider did not return a bounded final answer');
  }

  return {
    answer: secondChoice.message.content,
    tool: { name: call.function.name, result },
  };
}

async function handleRequest(request, response, { apiKey, authToken, fetchImpl, providerTimeoutMs }) {
  if (request.method === 'GET' && request.url === '/health') {
    writeJson(response, 200, { status: 'ok' });
    return;
  }
  if (request.method !== 'POST' || request.url !== '/chat') {
    writeJson(response, 404, { error: 'not found' });
    return;
  }
  if (!tokenMatches(request.headers.authorization, authToken)) {
    request.resume();
    writeJson(response, 401, { error: 'unauthorized' }, { 'WWW-Authenticate': 'Bearer' });
    return;
  }
  const contentType = request.headers['content-type'];
  if (typeof contentType !== 'string' || !/^application\/json(?:\s*;|$)/i.test(contentType)) {
    request.resume();
    writeJson(response, 415, { error: 'content type must be application/json' });
    return;
  }

  try {
    const body = await readJsonBody(request);
    const message = validateMessage(body?.message);
    const result = await runCalculatorAgent(message, { apiKey, fetchImpl, providerTimeoutMs });
    writeJson(response, 200, result);
  } catch (error) {
    if (error instanceof RequestError) {
      writeJson(response, error.status, { error: error.message });
      return;
    }
    // Do not log provider responses, user input, API keys, or demo tokens.
    writeJson(response, 502, { error: 'agent request failed' });
  }
}

export function createAgentServer({
  apiKey,
  authToken,
  fetchImpl = globalThis.fetch,
  providerTimeoutMs = PROVIDER_TIMEOUT_MS,
} = {}) {
  if (typeof apiKey !== 'string' || apiKey.length === 0) throw new Error('OPENROUTER_API_KEY is required');
  if (typeof authToken !== 'string' || authToken.length < 32) throw new Error('DEMO_AUTH_TOKEN must contain at least 32 characters');
  if (typeof fetchImpl !== 'function') throw new Error('fetch implementation is required');

  const server = createServer((request, response) => {
    void handleRequest(request, response, { apiKey, authToken, fetchImpl, providerTimeoutMs });
  });
  server.headersTimeout = 15_000;
  server.requestTimeout = 15_000;
  server.keepAliveTimeout = 5_000;
  return server;
}
