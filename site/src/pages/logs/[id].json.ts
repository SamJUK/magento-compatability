import type { APIRoute, GetStaticPaths } from 'astro';
import { STEPS, STEP_LABELS } from '@lib/compat.js';
import { getAllResults } from '@lib/results.js';
import type { TestResult } from '@lib/types.js';

// Logs are served per run rather than inlined in the pages: crawlers treat file paths inside
// inline JSON as relative links, and inlining them made version pages several megabytes.
export const getStaticPaths: GetStaticPaths = () => getAllResults().map((r) => ({ params: { id: r.id }, props: { r } }));

export const GET: APIRoute = ({ props }) => {
  const r = props.r as TestResult;
  const entry = {
    label: `${r.product} ${r.version} · PHP ${r.services.php} · ${r.services.db.type} ${r.services.db.version} · ${r.services.search.type} ${r.services.search.version}`,
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
