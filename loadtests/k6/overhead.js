// One case of the gateway overhead test: one path, one mode, one VU level.
// loadtests/run.sh runs every case and prints the table.
import http from 'k6/http';
import exec from 'k6/execution';
import { sleep } from 'k6';
import { Rate, Trend } from 'k6/metrics';

const TARGET = __ENV.TARGET; // direct or gateway
const MODE = __ENV.MODE; // stream or nonstream
const VUS = Number(__ENV.VUS);
const TOKENS = Number(__ENV.TOKENS);
const WARMUP = __ENV.WARMUP;
const DURATION = __ENV.DURATION;

const base = TARGET === 'gateway' ? 'http://gateway:8080' : 'http://fakeprovider:8081';
const model = TARGET === 'gateway' ? __ENV.ALIAS : __ENV.FAKE_MODEL;
const body = JSON.stringify({
  model,
  stream: MODE === 'stream',
  messages: [{ role: 'user', content: 'hi' }],
});
const params = { headers: { 'Content-Type': 'application/json' }, timeout: '60s' };

const words = ['The', ' quick', ' brown', ' fox', ' jumps', ' over', ' the', ' lazy', ' dog', '.'];
const expected = Array.from({ length: TOKENS }, (_, i) => words[i % words.length]).join('');

const duration = new Trend('chat_duration', true);
const failed = new Rate('chat_failed');

export const options = {
  scenarios: {
    // Fills connection pools and warms both servers; nothing it does is recorded.
    warmup: { executor: 'constant-vus', vus: VUS, duration: WARMUP, gracefulStop: '0s' },
    measure: { executor: 'constant-vus', vus: VUS, duration: DURATION, startTime: WARMUP, gracefulStop: '30s' },
  },
  setupTimeout: '90s',
  summaryTrendStats: ['med', 'p(95)', 'max', 'count'],
};

export function setup() {
  const want = TARGET === 'gateway' ? __ENV.ALIAS : 'fake';
  for (let i = 0; i < 60; i++) {
    const res = http.get(`${base}/v1/models`, { timeout: '2s' });
    if (res.status === 200 && (res.json('data') || []).some((m) => m.id === want)) {
      return;
    }
    sleep(1);
  }
  exec.test.abort(`${want} not listed at ${base}/v1/models after 60s`);
}

function ok(res) {
  if (res.status !== 200 || !res.body) {
    return false;
  }
  if (MODE === 'stream') {
    if (!res.body.endsWith('data: [DONE]\n\n') || res.body.includes('"error"')) {
      return false;
    }
    let text = '';
    for (const m of res.body.matchAll(/"content":"([^"]*)"/g)) {
      text += m[1];
    }
    return text === expected;
  }
  const c = res.json('choices.0');
  return c !== undefined && c.message !== undefined && c.message.content === expected && c.finish_reason === 'stop';
}

export default function () {
  const res = http.post(`${base}/v1/chat/completions`, body, params);
  if (exec.scenario.name !== 'measure') {
    return;
  }
  const good = ok(res);
  failed.add(!good);
  if (good) {
    duration.add(res.timings.duration);
  }
}

export function handleSummary(data) {
  const d = data.metrics.chat_duration ? data.metrics.chat_duration.values : {};
  const f = data.metrics.chat_failed ? data.metrics.chat_failed.values : {};
  const fmt = (v) => (v === undefined ? '-' : v.toFixed(2));
  const line = [TARGET, MODE, VUS, d.count || 0, fmt(d.med), fmt(d['p(95)']), fmt(d.max),
    f.rate === undefined ? '-' : (f.rate * 100).toFixed(2)].join('\t') + '\n';
  return { [`/results/${MODE}-${VUS}-${TARGET}.tsv`]: line, stdout: line };
}
