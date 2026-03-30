// k6 load test — Adaptive Rate Limiter
// Run: k6 run tests/load/rate_limit_test.js

import http from 'k6/http';
import { check, sleep } from 'k6';
import { Rate, Trend } from 'k6/metrics';

// Custom metrics
const blockedRate = new Rate('blocked_requests');
const latencyTrend = new Trend('request_latency', true);

export const options = {
    scenarios: {
        // Normal traffic
        steady_state: {
            executor: 'constant-arrival-rate',
            rate: 100,
            timeUnit: '1s',
            duration: '2m',
            preAllocatedVUs: 50,
            maxVUs: 200,
        },
        // Burst / attack simulation
        spike: {
            executor: 'ramping-arrival-rate',
            startRate: 100,
            timeUnit: '1s',
            stages: [
                { duration: '30s', target: 100 },
                { duration: '10s', target: 1000 },   // spike
                { duration: '30s', target: 1000 },   // sustained
                { duration: '10s', target: 100 },    // cool down
                { duration: '30s', target: 100 },
            ],
            preAllocatedVUs: 200,
            maxVUs: 500,
            startTime: '2m',
        },
    },
    thresholds: {
        http_req_duration: ['p(95)<500', 'p(99)<1000'],
        blocked_requests: ['rate<0.3'],
    },
};

const BASE_URL = __ENV.BASE_URL || 'http://localhost:80';

export default function () {
    const res = http.get(`${BASE_URL}/api/resource`, {
        headers: {
            'X-API-Key': `test-key-${__VU}`,
        },
    });

    latencyTrend.add(res.timings.duration);
    blockedRate.add(res.status === 429);

    check(res, {
        'status is 200 or 429': (r) => r.status === 200 || r.status === 429,
        'response time < 500ms': (r) => r.timings.duration < 500,
    });

    sleep(0.01);
}
