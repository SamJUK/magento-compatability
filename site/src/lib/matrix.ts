import { readFileSync } from 'node:fs';
import { resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import yaml from 'js-yaml';
import type {
  MatrixDefinition,
  ProductDefinition,
  ProductVersion,
  Baseline,
  WebserverDefinition,
  ServiceDefinition,
  TestServices,
} from './types.js';

const __dirname = dirname(fileURLToPath(import.meta.url));
const MATRIX_PATH = resolve(__dirname, '../../../matrix.yml');

// ─── Load + cache ─────────────────────────────────────────────────────────────

let _matrix: MatrixDefinition | null = null;

export function getMatrix(): MatrixDefinition {
  if (_matrix) return _matrix;
  const raw = readFileSync(MATRIX_PATH, 'utf-8');
  _matrix = yaml.load(raw) as MatrixDefinition;
  return _matrix;
}

export function resetCache(): void {
  _matrix = null;
}

// ─── Accessors ───────────────────────────────────────────────────────────────

export function getProducts(): ProductDefinition[] {
  return getMatrix().products;
}

export function getProductNames(): string[] {
  return getMatrix().products.map((p) => p.name);
}

export function getProduct(name: string): ProductDefinition | undefined {
  return getMatrix().products.find((p) => p.name === name);
}

export function getVersionsForProduct(productName: string): string[] {
  return getProduct(productName)?.versions.map((pv) => pv.version) ?? [];
}

export function getProductVersions(productName: string): ProductVersion[] {
  return getProduct(productName)?.versions ?? [];
}

export function getAllPhpVersions(): string[] {
  return getMatrix().services.php;
}

export function getAllWebservers(): WebserverDefinition[] {
  return getMatrix().services.webserver;
}

export function getAllDatabases(): ServiceDefinition[] {
  return getMatrix().services.database;
}

export function getAllSearchEngines(): ServiceDefinition[] {
  return getMatrix().services.search;
}

export function getAllCacheServices(): ServiceDefinition[] {
  return getMatrix().services.cache;
}

export function getAllQueueServices(): ServiceDefinition[] {
  return getMatrix().services.queue;
}

export function getAllVarnishVersions(): string[] {
  return getMatrix().services.varnish;
}

// ─── Service dimension helpers ────────────────────────────────────────────────

/** All distinct service type+version pairs across all dimensions */
export function getAllServiceDimensions(): Array<{ key: string; type: string; version: string; label: string }> {
  const m = getMatrix();
  const dims: Array<{ key: string; type: string; version: string; label: string }> = [];

  for (const v of m.services.php) {
    dims.push({ key: 'php', type: 'php', version: v, label: `PHP ${v}` });
  }
  for (const ws of m.services.webserver) {
    dims.push({ key: 'webserver', type: ws.type, version: ws.version, label: `${ws.type} ${ws.version}` });
  }
  for (const db of m.services.database) {
    dims.push({ key: 'db', type: db.type, version: db.version, label: `${db.type} ${db.version}` });
  }
  for (const s of m.services.search) {
    dims.push({ key: 'search', type: s.type, version: s.version, label: `${s.type} ${s.version}` });
  }
  for (const c of m.services.cache) {
    dims.push({ key: 'cache', type: c.type, version: c.version, label: `${c.type} ${c.version}` });
  }
  for (const q of m.services.queue) {
    dims.push({ key: 'queue', type: q.type, version: q.version, label: `${q.type} ${q.version}` });
  }
  for (const v of m.services.varnish) {
    dims.push({ key: 'varnish', type: 'varnish', version: v, label: v === 'none' ? 'No Varnish' : `Varnish ${v}` });
  }

  return dims;
}

// ─── Baseline-deviation combinations ─────────────────────────────────────────

/**
 * Generates all expected test combinations using the baseline-deviation strategy.
 * Optionally filtered to a specific product and/or version (for per-version counts).
 */
export function getAllCombinations(productFilter?: string, versionFilter?: string): TestServices[] {
  const m = getMatrix();
  const combos: TestServices[] = [];

  for (const p of m.products) {
    if (productFilter && p.name !== productFilter) continue;
    for (const pv of p.versions) {
      if (versionFilter && pv.version !== versionFilter) continue;
      if (!pv.baseline) continue;
      const bl: Baseline = pv.baseline;

      const blWS = m.services.webserver.find((ws) => ws.type === bl.webserver);
      if (!blWS) continue;

      const mk = (
        php: string,
        ws: WebserverDefinition,
        db: ServiceDefinition,
        search: ServiceDefinition,
        cache: ServiceDefinition,
        queue: ServiceDefinition,
        varnish: string
      ): TestServices => ({
        php,
        webserver: ws.type,
        db: { type: db.type, version: db.version },
        search: { type: search.type, version: search.version },
        cache: { type: cache.type, version: cache.version },
        queue: { type: queue.type, version: queue.version },
        varnish,
      });

      // Baseline
      combos.push(mk(bl.php, blWS, bl.db, bl.search, bl.cache, bl.queue, bl.varnish));
      // PHP deviations
      for (const php of m.services.php) {
        if (php !== bl.php) combos.push(mk(php, blWS, bl.db, bl.search, bl.cache, bl.queue, bl.varnish));
      }
      // Webserver deviations
      for (const ws of m.services.webserver) {
        if (ws.type !== bl.webserver) combos.push(mk(bl.php, ws, bl.db, bl.search, bl.cache, bl.queue, bl.varnish));
      }
      // Database deviations
      for (const db of m.services.database) {
        if (db.type !== bl.db.type || db.version !== bl.db.version) combos.push(mk(bl.php, blWS, db, bl.search, bl.cache, bl.queue, bl.varnish));
      }
      // Search deviations
      for (const search of m.services.search) {
        if (search.type !== bl.search.type || search.version !== bl.search.version) combos.push(mk(bl.php, blWS, bl.db, search, bl.cache, bl.queue, bl.varnish));
      }
      // Cache deviations
      for (const cache of m.services.cache) {
        if (cache.type !== bl.cache.type || cache.version !== bl.cache.version) combos.push(mk(bl.php, blWS, bl.db, bl.search, cache, bl.queue, bl.varnish));
      }
      // Queue deviations
      for (const queue of m.services.queue) {
        if (queue.type !== bl.queue.type || queue.version !== bl.queue.version) combos.push(mk(bl.php, blWS, bl.db, bl.search, bl.cache, queue, bl.varnish));
      }
      // Varnish deviations
      for (const varnish of m.services.varnish) {
        if (varnish !== bl.varnish) combos.push(mk(bl.php, blWS, bl.db, bl.search, bl.cache, bl.queue, varnish));
      }
    }
  }

  return combos;
}

/** Human-readable label for a service key + type */
export const SERVICE_GROUP_LABELS: Record<string, string> = {
  php: 'PHP',
  webserver: 'Webserver',
  db: 'Database',
  search: 'Search Engine',
  cache: 'Cache',
  queue: 'Message Queue',
  varnish: 'Varnish',
};
