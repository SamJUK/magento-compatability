import { readdirSync, readFileSync, existsSync } from 'node:fs';
import { resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import type {
  TestResult,
  StepResult,
  FailureInfo,
  UntestedResult,
  AnyResult,
  AggregatedServiceStatus,
  ServiceStatus,
  ServiceRowGroup,
  ServiceVersionEntry,
  VersionSummary,
  SoftwareVersionSummary,
  FailureOverview,
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
const STEP_ORDER = ['stack_up', 'install', 'smoke', 'playwright'] as const;

function formatServiceDelta(key: string, type: string, version: string): string {
  switch (key) {
    case 'php':
      return `PHP ${version}`;
    case 'webserver':
      return type;
    case 'db':
    case 'search':
    case 'cache':
    case 'queue':
      return `${type} ${version}`;
    case 'varnish':
      return version === 'none' ? 'No Varnish' : `Varnish ${version}`;
    default:
      return [type, version].filter(Boolean).join(' ');
  }
}

function buildVariantLabel(result: TestResult): string {
  const baseline = getProductVersions(result.product).find((pv) => pv.version === result.version)?.baseline;
  if (!baseline) {
    return result.version;
  }

  if (result.services.php !== baseline.php) {
    return formatServiceDelta('php', 'php', result.services.php);
  }
  if (result.services.webserver !== baseline.webserver) {
    return formatServiceDelta('webserver', result.services.webserver, '');
  }
  if (result.services.db.type !== baseline.db.type || result.services.db.version !== baseline.db.version) {
    return formatServiceDelta('db', result.services.db.type, result.services.db.version);
  }
  if (result.services.search.type !== baseline.search.type || result.services.search.version !== baseline.search.version) {
    return formatServiceDelta('search', result.services.search.type, result.services.search.version);
  }
  if (result.services.cache.type !== baseline.cache.type || result.services.cache.version !== baseline.cache.version) {
    return formatServiceDelta('cache', result.services.cache.type, result.services.cache.version);
  }
  if (result.services.queue.type !== baseline.queue.type || result.services.queue.version !== baseline.queue.version) {
    return formatServiceDelta('queue', result.services.queue.type, result.services.queue.version);
  }
  if (result.services.varnish !== baseline.varnish) {
    return formatServiceDelta('varnish', 'varnish', result.services.varnish);
  }

  return result.version;
}

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

export function getPrimaryFailure(result: TestResult): {
  stepName: typeof STEP_ORDER[number];
  step: StepResult;
  failure: FailureInfo | null;
} | null {
  for (const stepName of STEP_ORDER) {
    const step = result.steps?.[stepName];
    if (!step || step.status !== 'fail') {
      continue;
    }
    return {
      stepName,
      step,
      failure: step.failure ?? null,
    };
  }
  return null;
}

export function getFailureOverview(results: TestResult[]): FailureOverview {
  const failedRuns = results.filter((result) => result.overall_status === 'fail');
  const categoryCounts = new Map<string, number>();
  const buckets = new Map<string, FailureOverview['buckets'][number]>();

  let classifiedRuns = 0;
  let unclassifiedRuns = 0;
  let likelyFlakyRuns = 0;

  for (const result of failedRuns) {
    const primaryFailure = getPrimaryFailure(result);
    if (!primaryFailure?.failure) {
      unclassifiedRuns++;
      continue;
    }

    classifiedRuns++;
    const { stepName, failure } = primaryFailure;
    categoryCounts.set(failure.category, (categoryCounts.get(failure.category) ?? 0) + 1);
    if (failure.likely_flaky) {
      likelyFlakyRuns++;
    }

    const bucketKey = `${failure.category}\u0000${failure.code}\u0000${failure.summary}`;
    const existing = buckets.get(bucketKey);
    if (existing) {
      existing.count++;
      existing.likelyFlakyCount += failure.likely_flaky ? 1 : 0;
      if (!existing.stepNames.includes(stepName)) {
        existing.stepNames.push(stepName);
      }
      existing.resultIds.push(result.id);
      existing.variants.push({ id: result.id, label: buildVariantLabel(result) });
      continue;
    }

    buckets.set(bucketKey, {
      category: failure.category,
      code: failure.code,
      summary: failure.summary,
      count: 1,
      likelyFlakyCount: failure.likely_flaky ? 1 : 0,
      stepNames: [stepName],
      resultIds: [result.id],
      variants: [{ id: result.id, label: buildVariantLabel(result) }],
    });
  }

  return {
    totalFailedRuns: failedRuns.length,
    classifiedRuns,
    unclassifiedRuns,
    likelyFlakyRuns,
    categoryCounts: [...categoryCounts.entries()]
      .map(([category, count]) => ({ category, count }))
      .sort((a, b) => b.count - a.count || a.category.localeCompare(b.category)),
    buckets: [...buckets.values()]
      .sort((a, b) => b.count - a.count || a.summary.localeCompare(b.summary)),
  };
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
  const failureOverview = getFailureOverview(results);

  return {
    product,
    version,
    passCount,
    failCount,
    unknownCount,
    totalCombinations: totalExpected,
    serviceRows,
    lastTested,
    failureOverview,
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
  const failureOverview = getFailureOverview(results);
  return { total: results.length, passed, failed, lastRun, failureOverview };
}
