import type { APIRoute } from 'astro';
import { getAllResults } from '@lib/results.js';

export const GET: APIRoute = () => {
  const results = getAllResults();
  return new Response(JSON.stringify(results, null, 2), {
    headers: { 'Content-Type': 'application/json' },
  });
};
