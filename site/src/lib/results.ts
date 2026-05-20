import { readdirSync, readFileSync, existsSync } from 'node:fs';
import { resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import type {
  TestResult,
  UntestedResult,
  AnyResult,
  AggregatedServiceStatus,
  ServiceStatus,
  ServiceRowGroup,
  ServiceVersionEntry,
  VersionSummary,
  SoftwareVersionSummary,
} from './types.js';
import {
  getMatrix,
  getProductNames,
  getVersionsForProduct,
  getProductVersions,
  getAllCombinations,
  getAllWebservers,
  getAllDatabases,
  getAllSearchEngines,
  getAllCacheServices,
  getAllQueueServices,
  getAllVarnishVersions,
  SERVICE_GROUP_LABELS,
} from './matrix.js';

const __dirname = dirname(fileURLToPath(import.meta.url));
const RESULTS_BASE = resolve(__dirname, '../../../results');

// ─── Load all results from disk ──────────────────────────────────────────────

let _allResults: TestResult[] | null = null;

export function resetCache(): void {
  _allResults = null;
}

export function getAllResults(): TestResult[] {
  if (_allResults) return _allResults;
  const results: TestResult[] = [];

  for (const product of getProductNames()) {
    const dir = resolve(RESULTS_BASE, product);
    if (!existsSync(dir)) continue;

    for (const file of readdirSync(dir)) {
      if (!file.endsWith('.json') || file === '.gitkeep') continue;
      try {
        const raw = readFileSync(resolve(dir, file), 'utf-8');
        const parsed = JSON.parse(raw) as TestResult;
        if (parsed.id && parsed.product && parsed.version) {
          results.push(parsed);
        }
      } catch {
        // Skip malformed files
      }
    }
  }

  _allResults = results;
  return results;
}

// ─── Per-version results ──────────────────────────────────────────────────────

export function getResultsForVersion(product: string, version: string): TestResult[] {
  return getAllResults().filter((r) => r.product === product && r.version === version);
}

// ─── Aggregation helpers ──────────────────────────────────────────────────────

/** Extract the value for a given service key from a result's services object */
function getServiceValue(result: TestResult, key: string): { type: string; version: string } | null {
  const s = result.services;
  switch (key) {
    case 'php':
      return { type: 'php', version: s.php };
    case 'webserver':
      return { type: s.webserver, version: '' };
    case 'db':
      return s.db;
    case 'search':
      return s.search;
    case 'cache':
      return s.cache;
    case 'queue':
      return s.queue;
    case 'varnish':
      return { type: 'varnish', version: s.varnish };
    default:
      return null;
  }
}

/** Matches a result's service value against a target type+version */
function matchesService(result: TestResult, key: string, type: string, version: string): boolean {
  const val = getServiceValue(result, key);
  if (!val) return false;
  if (key === 'php') return val.version === version;
  if (key === 'webserver') return val.type === type;
  if (key === 'varnish') return val.version === version;
  return val.type === type && val.version === version;
}

export function aggregateResults(results: TestResult[]): AggregatedServiceStatus {
  if (results.length === 0) {
    return { status: 'unknown', passed: 0, failed: 0, total: 0, resultIds: [] };
  }
  const passed = results.filter((r) => r.overall_status === 'pass').length;
  const failed = results.length - passed;
  let status: ServiceStatus;
  if (failed === 0) status = 'pass';
  else if (passed === 0) status = 'fail';
  else status = 'pass';
//   else status = 'partial';

  return { status, passed, failed, total: results.length, resultIds: results.map((r) => r.id) };
}

export function aggregateByServiceVersion(
  results: TestResult[],
  key: string,
  type: string,
  version: string
): AggregatedServiceStatus {
  const matching = results.filter((r) => matchesService(r, key, type, version));
  return aggregateResults(matching);
}

// ─── Version summary (for the /[product]/[version] page) ─────────────────────

export function getVersionSummary(product: string, version: string): VersionSummary {
  const results = getResultsForVersion(product, version);
  const matrix = getMatrix();

  const serviceRows: ServiceRowGroup[] = [
    {
      label: 'PHP',
      key: 'php',
      entries: matrix.services.php.map((v) => ({
        key: 'php',
        type: 'php',
        version: v,
        label: `PHP ${v}`,
        aggregate: aggregateByServiceVersion(results, 'php', 'php', v),
      })),
    },
    {
      label: 'Database',
      key: 'db',
      entries: matrix.services.database.map((db) => ({
        key: 'db',
        type: db.type,
        version: db.version,
        label: `${db.type} ${db.version}`,
        aggregate: aggregateByServiceVersion(results, 'db', db.type, db.version),
      })),
    },
    {
      label: 'Search Engine',
      key: 'search',
      entries: matrix.services.search.map((s) => ({
        key: 'search',
        type: s.type,
        version: s.version,
        label: `${s.type} ${s.version}`,
        aggregate: aggregateByServiceVersion(results, 'search', s.type, s.version),
      })),
    },
    {
      label: 'Cache',
      key: 'cache',
      entries: matrix.services.cache.map((c) => ({
        key: 'cache',
        type: c.type,
        version: c.version,
        label: `${c.type} ${c.version}`,
        aggregate: aggregateByServiceVersion(results, 'cache', c.type, c.version),
      })),
    },
    {
      label: 'Message Queue',
      key: 'queue',
      entries: matrix.services.queue.map((q) => ({
        key: 'queue',
        type: q.type,
        version: q.version,
        label: `${q.type} ${q.version}`,
        aggregate: aggregateByServiceVersion(results, 'queue', q.type, q.version),
      })),
    },
    {
      label: 'Webserver',
      key: 'webserver',
      entries: matrix.services.webserver.map((ws) => ({
        key: 'webserver',
        type: ws.type,
        version: ws.version,
        label: `${ws.type} ${ws.version}`,
        aggregate: aggregateByServiceVersion(results, 'webserver', ws.type, ws.version),
      })),
    },
    {
      label: 'Varnish',
      key: 'varnish',
      entries: matrix.services.varnish.map((v) => ({
        key: 'varnish',
        type: 'varnish',
        version: v,
        label: v === 'none' ? 'No Varnish' : `Varnish ${v}`,
        aggregate: aggregateByServiceVersion(results, 'varnish', 'varnish', v),
      })),
    },
  ];

  const passCount = results.filter((r) => r.overall_status === 'pass').length;
  const failCount = results.filter((r) => r.overall_status === 'fail').length;
  const totalExpected = getAllCombinations(product, version).length;
  const unknownCount = totalExpected - results.length;

  const timestamps = results.map((r) => r.timestamp).filter(Boolean).sort();
  const lastTested = timestamps.length > 0 ? timestamps[timestamps.length - 1] : null;

  return {
    product,
    version,
    passCount,
    failCount,
    unknownCount,
    totalCombinations: totalExpected,
    serviceRows,
    lastTested,
  };
}

// ─── Software compatibility summary (for /software/[service]/[version]) ──────

export function getSoftwareVersionSummary(
  key: string,
  type: string,
  version: string
): SoftwareVersionSummary {
  const label = buildServiceLabel(key, type, version);
  const compatibility: SoftwareVersionSummary['compatibility'] = {};

  for (const product of getProductNames()) {
    for (const pv of getProductVersions(product)) {
      if (!pv.baseline) continue;
      const ver = pv.version;
      const results = getResultsForVersion(product, ver);
      const agg = aggregateByServiceVersion(results, key, type, version);
      compatibility[`${product}/${ver}`] = { product, version: ver, aggregate: agg };
    }
  }

  return { serviceType: type, serviceVersion: version, label, compatibility };
}

function buildServiceLabel(key: string, type: string, version: string): string {
  if (key === 'php') return `PHP ${version}`;
  if (key === 'varnish') return version === 'none' ? 'No Varnish' : `Varnish ${version}`;
  return `${type} ${version}`.trim();
}

// ─── All static paths for getStaticPaths() ───────────────────────────────────

/** Returns all {product, version} combos from the matrix */
export function getAllVersionPaths(): Array<{ product: string; version: string }> {
  const paths: Array<{ product: string; version: string }> = [];
  for (const product of getProductNames()) {
    for (const version of getVersionsForProduct(product)) {
      paths.push({ product, version });
    }
  }
  return paths;
}

/** Returns all {key, type, version} service dimension combos from the matrix */
export function getAllServicePaths(): Array<{ key: string; type: string; version: string; label: string }> {
  const matrix = getMatrix();
  const paths: Array<{ key: string; type: string; version: string; label: string }> = [];

  for (const v of matrix.services.php) {
    paths.push({ key: 'php', type: 'php', version: v, label: `PHP ${v}` });
  }
  for (const ws of matrix.services.webserver) {
    paths.push({ key: 'webserver', type: ws.type, version: ws.version, label: `${ws.type} ${ws.version}` });
  }
  for (const db of matrix.services.database) {
    paths.push({ key: 'db', type: db.type, version: db.version, label: `${db.type} ${db.version}` });
  }
  for (const s of matrix.services.search) {
    paths.push({ key: 'search', type: s.type, version: s.version, label: `${s.type} ${s.version}` });
  }
  for (const c of matrix.services.cache) {
    paths.push({ key: 'cache', type: c.type, version: c.version, label: `${c.type} ${c.version}` });
  }
  for (const q of matrix.services.queue) {
    paths.push({ key: 'queue', type: q.type, version: q.version, label: `${q.type} ${q.version}` });
  }
  for (const v of matrix.services.varnish) {
    paths.push({
      key: 'varnish',
      type: 'varnish',
      version: v,
      label: v === 'none' ? 'No Varnish' : `Varnish ${v}`,
    });
  }

  return paths;
}

// ─── Global stats ─────────────────────────────────────────────────────────────

export function getGlobalStats() {
  const results = getAllResults();
  const passed = results.filter((r) => r.overall_status === 'pass').length;
  const failed = results.filter((r) => r.overall_status === 'fail').length;
  const timestamps = results.map((r) => r.timestamp).filter(Boolean).sort();
  const lastRun = timestamps.length > 0 ? timestamps[timestamps.length - 1] : null;
  return { total: results.length, passed, failed, lastRun };
}
