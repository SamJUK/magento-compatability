import type { APIRoute, GetStaticPaths } from 'astro';
import { getProducts, getVersionsForProduct, getProductVersions } from '@lib/matrix.js';

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
  const pv = getProductVersions(product!).find((v) => v.version === version);
  if (!pv?.baseline) {
    return new Response(JSON.stringify({ error: 'No baseline' }), {
      status: 404,
      headers: { 'Content-Type': 'application/json' },
    });
  }
  const bl = pv.baseline;
  const matrix = {
    include: [{
      version: pv.version,
      php: bl.php,
      db_type: bl.db.type,
      db_version: bl.db.version,
      search_type: bl.search.type,
      search_version: bl.search.version,
      cache_type: bl.cache.type,
      cache_version: bl.cache.version,
      queue_type: bl.queue.type,
      queue_version: bl.queue.version,
      webserver: bl.webserver,
      varnish: bl.varnish,
    }],
  };
  return new Response(JSON.stringify(matrix, null, 2), {
    headers: { 'Content-Type': 'application/json' },
  });
};
