import type { APIRoute, GetStaticPaths } from 'astro';
import { getProducts, getVersionsForProduct } from '@lib/matrix.js';

export const getStaticPaths: GetStaticPaths = () => {
  return getProducts().map((p) => ({
    params: { product: p.name },
  }));
};

export const GET: APIRoute = ({ params }) => {
  const { product } = params;
  const versions = getVersionsForProduct(product!);
  return new Response(JSON.stringify({ product, versions }, null, 2), {
    headers: { 'Content-Type': 'application/json' },
  });
};
