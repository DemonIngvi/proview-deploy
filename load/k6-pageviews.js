// Load profile: one simulated page view = GET /v1/banner + POST /v1/events,
// so RPS requests per second = RPS/2 page views per second.
//
//   k6 run -e BASE=https://demo.proview.example -e RPS=20000 -e DURATION=10m load/k6-pageviews.js
//   docker run --rm --network host -v $PWD/load:/load grafana/k6 run -e RPS=2000 /load/k6-pageviews.js
//
// BANNER_BASE / EVENTS_BASE override BASE when the APIs live on other hosts.
import http from 'k6/http';
import { check } from 'k6';
import { Counter } from 'k6/metrics';

const BASE = __ENV.BASE || 'http://localhost:8080';
const BANNER = __ENV.BANNER_BASE || BASE;
const EVENTS = __ENV.EVENTS_BASE || BASE;
const RPS = Number(__ENV.RPS || 2000);
const DURATION = __ENV.DURATION || '1m';

const backpressure = new Counter('collector_backpressure_503');

export const options = {
  discardResponseBodies: false,
  scenarios: {
    pageviews: {
      executor: 'ramping-arrival-rate',
      startRate: Math.ceil(RPS / 20),
      timeUnit: '1s',
      preAllocatedVUs: Math.max(50, Math.ceil(RPS / 10)),
      maxVUs: Math.max(200, RPS),
      stages: [
        { target: RPS / 2, duration: '30s' },  // ramp up
        { target: RPS / 2, duration: DURATION }, // steady state
      ],
    },
  },
  thresholds: {
    http_req_failed: ['rate<0.01'],
    'http_req_duration{api:banner}': ['p(95)<50', 'p(99)<150'],
    'http_req_duration{api:events}': ['p(95)<50', 'p(99)<150'],
  },
};

const PLACEMENTS = ['home_top', 'sidebar', 'footer'];
const COUNTRIES = ['RU', 'RU', 'RU', 'KZ', 'BY', 'DE', 'US'];
const UAS = [
  'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/130.0 Safari/537.36',
  'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 Mobile/15E148',
];
const pick = (xs) => xs[Math.floor(Math.random() * xs.length)];

export default function () {
  const headers = { 'User-Agent': pick(UAS), 'X-Country': pick(COUNTRIES) };
  const res = http.get(`${BANNER}/v1/banner?placement=${pick(PLACEMENTS)}`, { headers, tags: { api: 'banner' } });
  if (!check(res, { 'banner 200/204': (r) => r.status === 200 || r.status === 204 }) || res.status !== 200) return;

  const now = Date.now();
  const events = [{ type: 'impression', ts: now, seq: 0 }];
  if (Math.random() < 0.7) {
    events.push({ type: 'viewable', ts: now + 1000, seq: 1 }, { type: 'active_time', ts: now + 5000, seq: 2, duration_ms: 5000 });
    if (Math.random() < 0.2) events.push({ type: 'hover', ts: now + 2000, seq: 3, duration_ms: 600 });
    if (Math.random() < 0.03) events.push({ type: 'click', ts: now + 3000, seq: 4 });
  }
  const body = JSON.stringify({
    visitor_id: `k6-${__VU}`, page_url: `${BASE}/`, items: [{ token: res.json('token'), events }],
  });
  const ev = http.post(`${EVENTS}/v1/events`, body, {
    headers: { ...headers, 'Content-Type': 'text/plain;charset=UTF-8' }, tags: { api: 'events' },
  });
  if (ev.status === 503) backpressure.add(1);
  check(ev, { 'events 202': (r) => r.status === 202 });
}
