import { defineConfig, fontProviders } from 'astro/config';
import tailwindcss from '@tailwindcss/vite';
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
        const compat  = await server.ssrLoadModule('/src/lib/compat.ts');
        matrix.resetCache?.();
        results.resetCache?.();
        compat.resetCache?.();
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
  site: 'https://magento.works',
  fonts: [
    {
      provider: fontProviders.fontsource(),
      name: 'Bricolage Grotesque',
      cssVariable: '--font-bricolage',
      weights: [500, 700, 800],
      subsets: ['latin'],
      fallbacks: ['system-ui', 'sans-serif'],
    },
    {
      provider: fontProviders.fontsource(),
      name: 'IBM Plex Sans',
      cssVariable: '--font-plex-sans',
      weights: [400, 500, 600, 700],
      subsets: ['latin'],
      fallbacks: ['system-ui', 'sans-serif'],
    },
    {
      provider: fontProviders.fontsource(),
      name: 'IBM Plex Mono',
      cssVariable: '--font-plex-mono',
      weights: [400, 500, 600],
      subsets: ['latin'],
      fallbacks: ['ui-monospace', 'monospace'],
    },
  ],
  vite: {
    plugins: [tailwindcss(), devCacheRefreshPlugin],
    resolve: {
      alias: {
        '@lib': path.resolve(__dirname, 'src/lib'),
        '@components': path.resolve(__dirname, 'src/components'),
      },
    },
  },
});
