import type { APIRoute, GetStaticPaths } from 'astro';
import { firstFailure, option, PRODUCT_LABELS, STEPS, STEP_LABELS } from '@lib/compat.js';
import { getAllResults } from '@lib/results.js';
import { neededWorkarounds } from '@lib/workarounds.js';
import type { TestResult } from '@lib/types.js';

// Logs are served per run rather than inlined in the pages: crawlers treat file paths inside
// inline JSON as relative links, and inlining them made version pages several megabytes.
export const getStaticPaths: GetStaticPaths = () => getAllResults().map((r) => ({ params: { id: r.id }, props: { r } }));

export const GET: APIRoute = ({ props }) => {
  const r = props.r as TestResult;
  const s = r.services;
  const failure = r.overall_status === 'pass' ? undefined : firstFailure(r);
  const entry = {
    release: `${PRODUCT_LABELS[r.product] ?? r.product} ${r.version}`,
    href: `/${r.product}/${r.version}`,
    status: r.overall_status,
    failure: failure && { step: STEP_LABELS[failure.step], summary: failure.summary },
    extra: neededWorkarounds(r.workarounds).map((w) => w.title),
    stack: [
      option('php', 'php', s.php),
      option('db', s.db.type, s.db.version),
      option('search', s.search.type, s.search.version),
      option('cache', s.cache.type, s.cache.version),
      option('queue', s.queue.type, s.queue.version),
      option('webserver', s.webserver, ''),
      option('varnish', 'varnish', s.varnish),
    ].map((o) => o.label),
    timestamp: r.timestamp,
    steps: STEPS.filter((s) => r.steps?.[s]).map((s) => ({
      name: STEP_LABELS[s],
      status: r.steps[s].status,
      duration: r.steps[s].duration_s,
      log: r.steps[s].log,
    })),
    containers: Object.entries(r.container_logs ?? {}).map(([name, log]) => ({ name, log })),
  };
  return new Response(JSON.stringify(entry), { headers: { 'Content-Type': 'application/json' } });
};
