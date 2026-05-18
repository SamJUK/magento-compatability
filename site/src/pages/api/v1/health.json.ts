import type { APIRoute } from 'astro';
import { getGlobalStats } from '@lib/results.js';

export const GET: APIRoute = () => {
  const stats = getGlobalStats();
  const health = {
    status: 'ok',
    total: stats.total,
    passed: stats.passed,
    failed: stats.failed,
    lastRun: stats.lastRun,
    generatedAt: new Date().toISOString(),
  };
  return new Response(JSON.stringify(health, null, 2), {
    headers: { 'Content-Type': 'application/json' },
  });
};
