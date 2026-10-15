/**
 * Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
 * SPDX-License-Identifier: OSL-3.0
 */

import type { APIRoute } from 'astro';
import { getMatrix } from '@lib/matrix.js';

export const GET: APIRoute = () => {
  const matrix = getMatrix();
  return new Response(JSON.stringify(matrix, null, 2), {
    headers: { 'Content-Type': 'application/json' },
  });
};
