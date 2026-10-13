// Sends streamed chat requests to the gateway's local route at a fixed
// concurrency. The gateway's log lines and metrics hold the time to
// first token; k6 only creates the load.
import { check } from 'k6';
import http from 'k6/http';

const LEVEL = Number(__ENV.LEVEL); // concurrent requests
const ROUNDS = Number(__ENV.ROUNDS); // requests per virtual user

// About 200 tokens, so prefill work is the same for every request.
const passage = `A lighthouse keeper's day followed the light, not the clock. Before sunset the keeper climbed
  the tower, trimmed the wick, cleaned soot from the lens and wound the clockwork that turned it. Through the night
  the keeper checked the flame every few hours and wrote the weather, passing ships and any fault in the log. A
  lens could focus a small flame into a beam seen more than twenty miles away, so a clean lens mattered more than a
  bright flame. At dawn the keeper put out the light, covered the lens to protect it from the sun, and began the
  day's maintenance: paint, brass, windows and the fuel store. Supplies came by boat, and storms could delay them
  for weeks, so each station kept spare parts and food. Electric lamps and automatic controls replaced most keepers
  during the twentieth century, and many towers now run with no one on site.`;

const params = { headers: { 'Content-Type': 'application/json' }, timeout: '180s' };

export const options = {
	scenarios: {
		run: { executor: 'per-vu-iterations', vus: LEVEL, iterations: ROUNDS, maxDuration: '30m' },
	},
	thresholds: { checks: ['rate==1'] },
};

export default function () {
	// The unique ID comes first: a prefix cache cannot reuse a prompt
	// whose first token differs.
	const id = `Ref ${__VU}-${__ITER}-${Date.now()}`;
	const body = JSON.stringify({
		model: 'local',
		stream: true,
		stream_options: { include_usage: true },
		max_tokens: 128,
		temperature: 0,
		messages: [{
			role: 'user', content: `${id}\n\nSummarize this text in one paragraph of about 150 words.\n\n${passage}`
		}],
	});
	const r = http.post('http://gateway:8080/v1/chat/completions', body, params);
	check(r, {
		'status 200': (res) => res.status === 200,
		'stream complete': (res) => res.body.includes('data: [DONE]'),
	});
}