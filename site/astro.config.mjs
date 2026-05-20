import { defineConfig } from 'astro/config';
import tailwind from '@astrojs/tailwind';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const __dirname = path.dirname(fileURLToPath(import.meta.url));

// Dev-only Vite plugin: POST /_refresh clears the in-memory matrix + results
// caches so the next page render re-reads from disk — no server restart needed.
const devCacheRefreshPlugin = {
  name: 'dev-cache-refresh',
  apply: 'serve',
  configureServer(server) {
    server.middlewares.use('/_refresh', async (req, res) => {
      if (req.method !== 'POST') {
        res.statusCode = 405;
        res.end();
        return;
      }
      try {
        const matrix  = await server.ssrLoadModule('/src/lib/matrix.ts');
        const results = await server.ssrLoadModule('/src/lib/results.ts');
        matrix.resetCache?.();
        results.resetCache?.();
        res.setHeader('Content-Type', 'application/json');
        res.end(JSON.stringify({ ok: true }));
      } catch (e) {
        res.statusCode = 500;
        res.setHeader('Content-Type', 'application/json');
        res.end(JSON.stringify({ ok: false, error: String(e) }));
      }
    });
  },
};

export default defineConfig({
  output: 'static',
  integrations: [tailwind()],
  site: 'https://m2compat.example.com',
  vite: {
    plugins: [devCacheRefreshPlugin],
    resolve: {
      alias: {
        '@lib': path.resolve(__dirname, 'src/lib'),
        '@components': path.resolve(__dirname, 'src/components'),
      },
    },
  },
});
