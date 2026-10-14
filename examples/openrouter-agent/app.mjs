import { timingSafeEqual } from 'node:crypto';
import { createServer } from 'node:http';
import { isIP } from 'node:net';

export const MODEL_ID = 'anthropic/claude-sonnet-5.5';
export const OPENROUTER_CHAT_URL = 'https://openrouter.ai/api/v1/chat/completions';
export const OPENROUTER_RELAY_URL = 'http://openrouter-demo-relay:8080/api/v1/chat/completions';
export const MAX_BODY_BYTES = 4096;
export const MAX_MESSAGE_LENGTH = 1000;
export const MAX_OUTPUT_TOKENS = 512;
export const PROVIDER_TIMEOUT_MS = 25_000;

const MAX_NUMBER_ABS = 1_000_000;
const MAX_PROVIDER_BODY_BYTES = 64 * 1024;
const MAX_ANSWER_LENGTH = 4000;
const FAILURE_CODES = new Set([
  'provider_timeout',
  'provider_transport_error',
  'provider_http_error',
  'provider_response_too_large',
  'provider_invalid_json',
  'provider_invalid_completion',
  'provider_tool_call_missing',
  'provider_tool_call_truncated',
  'provider_tool_call_invalid',
  'provider_tool_arguments_invalid',
  'provider_tool_arguments_out_of_range',
  'provider_tool_result_out_of_range',
  'provider_final_answer_truncated',
  'provider_final_answer_invalid',
]);
const FINISH_REASONS = new Set(['stop', 'tool_calls', 'length', 'content_filter', 'function_call']);
const toolSpec = [{
  type: 'function',
  function: {
    name: 'add_numbers',
    description: 'Add two finite numbers, each between -1000000 and 1000000.',
    strict: true,
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

class AgentFailure extends Error {
  constructor(code, details = {}) {
    super(code);
    this.code = code;
    this.providerStatus = details.providerStatus;
    this.finishReason = details.finishReason;
  }
}

function sanitizedFailure(error) {
  const record = {
    event: 'openrouter_agent_failure',
    code: error instanceof AgentFailure && FAILURE_CODES.has(error.code)
      ? error.code
      : 'agent_internal_error',
  };
  if (Number.isInteger(error?.providerStatus) && error.providerStatus >= 100 && error.providerStatus <= 599) {
    record.provider_status = error.providerStatus;
  }
  if (error?.finishReason !== undefined) {
    record.provider_finish_reason = FINISH_REASONS.has(error.finishReason)
      ? error.finishReason
      : 'other';
  }
  return record;
}

function writeFailureDiagnostic(record) {
  process.stderr.write(`${JSON.stringify(record)}\n`);
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

function relayURL(relayIP) {
  if (relayIP === undefined) return OPENROUTER_RELAY_URL;
  if (isIP(relayIP) !== 4) throw new Error('PROVIDER_RELAY_IP must be a private IPv4 literal in the tenant Docker pool');
  const [first, second, , last] = relayIP.split('.').map(Number);
  if (first !== 172 || second < 20 || second > 23 || last < 2 || last > 254) {
    throw new Error('PROVIDER_RELAY_IP must be a private IPv4 literal in the tenant Docker pool');
  }
  return `http://${relayIP}:8080/api/v1/chat/completions`;
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
    throw new AgentFailure('provider_tool_call_invalid');
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
    throw new AgentFailure('provider_tool_call_invalid');
  }

  let args;
  try {
    args = JSON.parse(call.function.arguments);
  } catch {
    throw new AgentFailure('provider_tool_arguments_invalid');
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
    throw new AgentFailure('provider_tool_arguments_out_of_range');
  }
  return { id: call.id, args, call };
}

async function requestCompletion(fetchImpl, providerURL, providerToken, messages, signal, toolChoice) {
  let response;
  try {
    response = await fetchImpl(providerURL, {
      method: 'POST',
      headers: {
        Authorization: `Bearer ${providerToken}`,
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
  } catch (error) {
    throw new AgentFailure(
      error?.name === 'TimeoutError' || error?.name === 'AbortError'
        ? 'provider_timeout'
        : 'provider_transport_error',
    );
  }

  if (!response.ok) {
    throw new AgentFailure('provider_http_error', { providerStatus: response.status });
  }
  let responseText;
  try {
    responseText = await response.text();
  } catch (error) {
    throw new AgentFailure(
      error?.name === 'TimeoutError' || error?.name === 'AbortError'
        ? 'provider_timeout'
        : 'provider_transport_error',
    );
  }
  if (Buffer.byteLength(responseText, 'utf8') > MAX_PROVIDER_BODY_BYTES) {
    throw new AgentFailure('provider_response_too_large');
  }
  let data;
  try {
    data = JSON.parse(responseText);
  } catch {
    throw new AgentFailure('provider_invalid_json');
  }
  if (data?.error || !Array.isArray(data?.choices) || !data.choices[0] || typeof data.choices[0] !== 'object') {
    throw new AgentFailure('provider_invalid_completion');
  }
  return data.choices[0];
}

async function runCalculatorAgent(message, { providerURL, providerToken, fetchImpl, providerTimeoutMs }) {
  const signal = AbortSignal.timeout(providerTimeoutMs);
  const messages = [
    {
      role: 'system',
      content: 'You are a calculator assistant. When asked to add two numbers, you must call add_numbers exactly once, then explain the result briefly. Do not calculate the sum mentally or claim a result before using the tool.',
    },
    { role: 'user', content: message },
  ];

  const firstChoice = await requestCompletion(fetchImpl, providerURL, providerToken, messages, signal, 'auto');
  if (firstChoice?.finish_reason !== 'tool_calls' || !firstChoice.message) {
    if (firstChoice?.finish_reason === 'length') {
      throw new AgentFailure('provider_tool_call_truncated', { finishReason: firstChoice.finish_reason });
    }
    if (firstChoice?.finish_reason === 'stop') {
      throw new AgentFailure('provider_tool_call_missing', { finishReason: firstChoice.finish_reason });
    }
    throw new AgentFailure('provider_invalid_completion', { finishReason: firstChoice?.finish_reason });
  }
  const { id, args, call } = validateToolCall(firstChoice.message);
  const result = args.a + args.b;
  if (!Number.isFinite(result)) throw new AgentFailure('provider_tool_result_out_of_range');

  const assistantToolMessage = {
    role: 'assistant',
    content: firstChoice.message.content ?? null,
    tool_calls: firstChoice.message.tool_calls,
  };
  if (firstChoice.message.reasoning_details !== undefined) {
    assistantToolMessage.reasoning_details = firstChoice.message.reasoning_details;
  }

  const secondChoice = await requestCompletion(fetchImpl, providerURL, providerToken, [
    ...messages,
    assistantToolMessage,
    { role: 'tool', tool_call_id: id, content: String(result) },
  ], signal, 'none');
  if (secondChoice?.finish_reason === 'length') {
    throw new AgentFailure('provider_final_answer_truncated', { finishReason: secondChoice.finish_reason });
  }
  if (
    secondChoice?.finish_reason !== 'stop' ||
    !secondChoice.message ||
    secondChoice.message.tool_calls?.length ||
    typeof secondChoice.message.content !== 'string' ||
    secondChoice.message.content.trim().length === 0 ||
    secondChoice.message.content.length > MAX_ANSWER_LENGTH
  ) {
    throw new AgentFailure('provider_final_answer_invalid', { finishReason: secondChoice?.finish_reason });
  }

  return {
    answer: secondChoice.message.content,
    tool: { name: call.function.name, result },
  };
}

async function handleRequest(request, response, {
  providerURL,
  providerToken,
  authToken,
  fetchImpl,
  providerTimeoutMs,
  diagnosticSink,
}) {
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
    const result = await runCalculatorAgent(message, {
      providerURL, providerToken, fetchImpl, providerTimeoutMs,
    });
    writeJson(response, 200, result);
  } catch (error) {
    if (error instanceof RequestError) {
      writeJson(response, error.status, { error: error.message });
      return;
    }
    try {
      diagnosticSink(sanitizedFailure(error));
    } catch {
      // Diagnostics must never affect the generic public response.
    }
    writeJson(response, 502, { error: 'agent request failed' });
  }
}

export function createAgentServer({
  apiKey,
  relayToken,
  relayIP,
  authToken,
  fetchImpl = globalThis.fetch,
  providerTimeoutMs = PROVIDER_TIMEOUT_MS,
  diagnosticSink = writeFailureDiagnostic,
} = {}) {
  const useRelay = typeof relayToken === 'string' && relayToken.length > 0;
  if (relayIP !== undefined && !useRelay) {
    throw new Error('PROVIDER_RELAY_IP requires PROVIDER_RELAY_TOKEN');
  }
  if (!useRelay && (typeof apiKey !== 'string' || apiKey.length === 0)) {
    throw new Error('OPENROUTER_API_KEY is required when PROVIDER_RELAY_TOKEN is not set');
  }
  if (useRelay && relayToken.length < 32) throw new Error('PROVIDER_RELAY_TOKEN must contain at least 32 characters');
  if (useRelay && apiKey !== undefined) {
    throw new Error('OPENROUTER_API_KEY must not be set in provider relay mode');
  }
  if (typeof authToken !== 'string' || authToken.length < 32) throw new Error('DEMO_AUTH_TOKEN must contain at least 32 characters');
  if (typeof fetchImpl !== 'function') throw new Error('fetch implementation is required');
  if (typeof diagnosticSink !== 'function') throw new Error('diagnostic sink must be a function');
  const providerURL = useRelay ? relayURL(relayIP) : OPENROUTER_CHAT_URL;

  const server = createServer((request, response) => {
    void handleRequest(request, response, {
      providerURL,
      providerToken: useRelay ? relayToken : apiKey,
      authToken,
      fetchImpl,
      providerTimeoutMs,
      diagnosticSink,
    });
  });
  server.headersTimeout = 15_000;
  server.requestTimeout = 15_000;
  server.keepAliveTimeout = 5_000;
  return server;
}
