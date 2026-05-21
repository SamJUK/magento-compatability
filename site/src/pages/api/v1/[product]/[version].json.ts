import type { APIRoute, GetStaticPaths } from 'astro';
import { getProducts, getVersionsForProduct } from '@lib/matrix.js';
import { getResultsForVersion, getVersionSummary } from '@lib/results.js';

export const getStaticPaths: GetStaticPaths = () => {
  const paths = [];
  for (const product of getProducts()) {
    for (const version of getVersionsForProduct(product.name)) {
      paths.push({ params: { product: product.name, version } });
    }
  }
  return paths;
};

export const GET: APIRoute = ({ params }) => {
  const { product, version } = params;
  const results = getResultsForVersion(product!, version!);
  const summary = getVersionSummary(product!, version!);

  const strippedResults = results.map((r) => {
    const { steps, container_logs, ...rest } = r;
    return rest;
  });

  return new Response(JSON.stringify({ product, version, summary, strippedResults }, null, 2), {
    headers: { 'Content-Type': 'application/json' },
  });
};
