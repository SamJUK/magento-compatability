import type { APIRoute, GetStaticPaths } from 'astro';
import { getProducts, getProductVersions } from '@lib/matrix.js';

export const getStaticPaths: GetStaticPaths = () =>
  getProducts().map((p) => ({ params: { product: p.name } }));

export const GET: APIRoute = ({ params }) => {
  const { product } = params;
  const matrix = {
    include: getProductVersions(product!)
      .filter((pv) => pv.baseline)
      .map((pv) => {
        const bl = pv.baseline!;
        return {
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
        };
      }),
  };
  return new Response(JSON.stringify(matrix, null, 2), {
    headers: { 'Content-Type': 'application/json' },
  });
};
