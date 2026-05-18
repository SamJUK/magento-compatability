import type { APIRoute } from 'astro';
import { getMatrix } from '@lib/matrix.js';

export const GET: APIRoute = () => {
  const matrix = getMatrix();
  return new Response(JSON.stringify(matrix, null, 2), {
    headers: { 'Content-Type': 'application/json' },
  });
};
