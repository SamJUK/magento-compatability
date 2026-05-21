import type { APIRoute } from 'astro';
import { getAllResults } from '@lib/results.js';

export const GET: APIRoute = () => {
  const strippedResults = getAllResults().map((r) => {
    const { steps, container_logs, ...rest } = r;
    return rest;
  });

  return new Response(JSON.stringify(strippedResults), {
    headers: { 'Content-Type': 'application/json' },
  });
};
