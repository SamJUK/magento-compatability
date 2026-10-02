import { request, FullConfig } from '@playwright/test';

const pages = ['/', '/e2e-checkout-product.html', process.env.PLAYWRIGHT_ADMIN_PATH ?? '/admin'];

// Developer mode compiles LESS on first request, and two compiles in parallel
// race on var/tmp/alternative-source-css.lock: the loser 404s and the page
// renders unstyled. Compile every stylesheet the specs need one at a time first.
export default async function globalSetup(config: FullConfig): Promise<void> {
  const api = await request.newContext({ baseURL: config.projects[0].use.baseURL });
  const stylesheets = new Set<string>();

  for (const path of pages) {
    const html = await (await api.get(path)).text();
    for (const [, href] of html.matchAll(/<link[^>]+href="([^"]+\.css)"/g)) {
      stylesheets.add(href);
    }
  }

  if (stylesheets.size === 0) {
    throw new Error(`No stylesheets linked from ${pages.join(', ')}`);
  }
  console.log(`Compiling ${stylesheets.size} stylesheets before the specs run`);

  for (const href of stylesheets) {
    const response = await api.get(href, { timeout: 180_000 });
    if (!response.ok()) {
      throw new Error(`${href} returned ${response.status()}: ${(await response.text()).slice(0, 500)}`);
    }
  }

  await api.dispose();
}
