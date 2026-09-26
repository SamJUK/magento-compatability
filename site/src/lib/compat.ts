import { getMatrix, getProductVersions } from './matrix.js';
import { getResultsForVersion } from './results.js';
import type { Baseline, StepResult, TestResult, TestServices } from './types.js';

export type DimKey = 'php' | 'db' | 'search' | 'cache' | 'queue' | 'webserver' | 'varnish';
export type CellStatus = 'pass' | 'fail' | 'untested';

export interface ServiceOption {
  id: string;
  key: DimKey;
  type: string;
  version: string;
  label: string;
  short: string;
  href: string;
}

export interface Dimension {
  key: DimKey;
  label: string;
  options: ServiceOption[];
}

export interface Failure {
  step: StepName;
  code: string;
  category: string;
  summary: string;
}

export interface Cell {
  option: ServiceOption;
  status: CellStatus;
  recommended: boolean;
  result?: TestResult;
  failure?: Failure;
}

export const STEPS = ['stack_up', 'install', 'smoke', 'playwright'] as const;
export type StepName = (typeof STEPS)[number];

export const STEP_LABELS: Record<StepName, string> = {
  stack_up: 'Stack up',
  install: 'Install',
  smoke: 'Compile',
  playwright: 'Browser tests',
};

export const PRODUCT_LABELS: Record<string, string> = {
  magento: 'Magento',
  mageos: 'Mage-OS',
};

const TYPE_LABELS: Record<string, string> = {
  mariadb: 'MariaDB',
  mysql: 'MySQL',
  percona: 'Percona',
  elasticsearch: 'Elasticsearch',
  opensearch: 'OpenSearch',
  redis: 'Redis',
  valkey: 'Valkey',
  rabbitmq: 'RabbitMQ',
  apache: 'Apache',
  nginx: 'Nginx',
};

const SHORT_TYPE_LABELS: Record<string, string> = {
  elasticsearch: 'ES',
  opensearch: 'OS',
  mariadb: 'Maria',
};

const DIM_LABELS: Record<DimKey, string> = {
  php: 'PHP',
  db: 'Database',
  search: 'Search',
  cache: 'Cache',
  queue: 'Queue',
  webserver: 'Web server',
  varnish: 'Varnish',
};

function option(key: DimKey, type: string, version: string): ServiceOption {
  const typeLabel = TYPE_LABELS[type] ?? type;
  let label: string;
  let short: string;
  if (key === 'php') {
    label = `PHP ${version}`;
    short = version;
  } else if (key === 'varnish') {
    label = version === 'none' ? 'No Varnish' : `Varnish ${version}`;
    short = version === 'none' ? 'none' : version;
  } else if (key === 'webserver') {
    label = typeLabel;
    short = typeLabel;
  } else {
    label = `${typeLabel} ${version}`;
    short = `${SHORT_TYPE_LABELS[type] ?? typeLabel} ${version.split('.').slice(0, 2).join('.')}`;
  }
  return {
    id: `${key}:${valueOf(key, type, version)}`,
    key,
    type,
    version,
    label,
    short,
    href: `/software/${key}/${type}-${version}`,
  };
}

function valueOf(key: DimKey, type: string, version: string): string {
  if (key === 'php' || key === 'varnish') return version;
  if (key === 'webserver') return type;
  return `${type}-${version}`;
}

function serviceValue(services: TestServices | Baseline, key: DimKey): string {
  switch (key) {
    case 'php':
      return services.php;
    case 'varnish':
      return services.varnish;
    case 'webserver':
      return services.webserver;
    default:
      return `${services[key].type}-${services[key].version}`;
  }
}

let _dimensions: Dimension[] | null = null;

export function getDimensions(): Dimension[] {
  if (_dimensions) return _dimensions;
  const s = getMatrix().services;
  _dimensions = [
    { key: 'php', options: s.php.map((v) => option('php', 'php', v)) },
    { key: 'db', options: s.database.map((d) => option('db', d.type, d.version)) },
    { key: 'search', options: s.search.map((d) => option('search', d.type, d.version)) },
    { key: 'cache', options: s.cache.map((d) => option('cache', d.type, d.version)) },
    { key: 'queue', options: s.queue.map((d) => option('queue', d.type, d.version)) },
    { key: 'webserver', options: s.webserver.map((d) => option('webserver', d.type, d.version)) },
    { key: 'varnish', options: s.varnish.map((v) => option('varnish', 'varnish', v)) },
  ].map((d) => ({
    key: d.key as DimKey,
    label: DIM_LABELS[d.key as DimKey],
    options: [...d.options].sort((a, b) => compareOptions(a, b)),
  }));
  return _dimensions;
}

export function getOption(key: string, slug: string): ServiceOption | undefined {
  return getDimensions()
    .find((d) => d.key === key)
    ?.options.find((o) => `${o.type}-${o.version}` === slug);
}

function compareOptions(a: ServiceOption, b: ServiceOption): number {
  if (a.type !== b.type) return a.type.localeCompare(b.type);
  return compareVersions(a.version, b.version);
}

/** Ascending; `2.4.8-p5` sorts after `2.4.8`, numeric segments compare as numbers. */
export function compareVersions(a: string, b: string): number {
  const parse = (v: string) => {
    const [main, patch] = v.split('-p');
    return [...main.split('.').map(Number), patch ? Number(patch) : 0];
  };
  const pa = parse(a);
  const pb = parse(b);
  for (let i = 0; i < Math.max(pa.length, pb.length); i++) {
    const diff = (pa[i] ?? 0) - (pb[i] ?? 0);
    if (diff !== 0) return diff;
  }
  return 0;
}

/** Magento groups by minor (`2.4.8`), Mage-OS by major (`2.x`). */
export function lineOf(product: string, version: string): string {
  if (product === 'magento') return version.split('-p')[0];
  return `${version.split('.')[0]}.x`;
}

export function patchLabel(product: string, version: string): string {
  if (product !== 'magento') return version;
  const patch = version.split('-p')[1];
  return patch ? `p${patch}` : 'base';
}

export interface ReleaseInfo {
  product: string;
  version: string;
  line: string;
  baseline: Baseline;
}

/** Releases with a baseline, newest first. */
export function getReleases(product: string): ReleaseInfo[] {
  return getProductVersions(product)
    .filter((pv) => pv.baseline)
    .map((pv) => ({ product, version: pv.version, line: lineOf(product, pv.version), baseline: pv.baseline! }))
    .sort((a, b) => compareVersions(b.version, a.version));
}

export function getLines(product: string): Array<{ line: string; releases: ReleaseInfo[] }> {
  const lines = new Map<string, ReleaseInfo[]>();
  for (const r of getReleases(product)) {
    if (!lines.has(r.line)) lines.set(r.line, []);
    lines.get(r.line)!.push(r);
  }
  return [...lines].map(([line, releases]) => ({ line, releases: releases.reverse() }));
}

export function getRelease(product: string, version: string): ReleaseInfo | undefined {
  return getReleases(product).find((r) => r.version === version);
}

export function firstFailure(result: TestResult): Failure | undefined {
  for (const step of STEPS) {
    const s: StepResult | undefined = result.steps?.[step];
    if (!s || s.status === 'pass') continue;
    return {
      step,
      code: s.failure?.code ?? `${step}_failed`,
      category: s.failure?.category ?? 'unknown',
      summary: s.failure?.summary ?? `Failed during ${STEP_LABELS[step].toLowerCase()}.`,
    };
  }
  return undefined;
}

const _cells = new Map<string, Map<string, Cell>>();

/**
 * One cell per service option: the run that used the recommended stack with only
 * that service swapped, or the recommended run itself for the recommended option.
 */
export function getCells(product: string, version: string): Map<string, Cell> {
  const cacheKey = `${product}/${version}`;
  const cached = _cells.get(cacheKey);
  if (cached) return cached;

  const release = getRelease(product, version);
  const cells = new Map<string, Cell>();
  const dims = getDimensions();
  const byOption = new Map<string, TestResult>();

  if (release) {
    const newer = (a: TestResult, b?: TestResult) => !b || a.timestamp > b.timestamp;
    for (const result of getResultsForVersion(product, version)) {
      const diff = dims.filter((d) => serviceValue(result.services, d.key) !== serviceValue(release.baseline, d.key));
      if (diff.length === 0) {
        if (newer(result, byOption.get('baseline'))) byOption.set('baseline', result);
      } else if (diff.length === 1) {
        const id = `${diff[0].key}:${serviceValue(result.services, diff[0].key)}`;
        if (newer(result, byOption.get(id))) byOption.set(id, result);
      }
    }
  }

  for (const dim of dims) {
    for (const opt of dim.options) {
      const recommended = !!release && `${dim.key}:${serviceValue(release.baseline, dim.key)}` === opt.id;
      const result = byOption.get(recommended ? 'baseline' : opt.id);
      cells.set(opt.id, {
        option: opt,
        recommended,
        result,
        status: !result ? 'untested' : result.overall_status === 'pass' ? 'pass' : 'fail',
        failure: result && result.overall_status !== 'pass' ? firstFailure(result) : undefined,
      });
    }
  }

  _cells.set(cacheKey, cells);
  return cells;
}

export function getBaselineResult(product: string, version: string): TestResult | undefined {
  const cells = getCells(product, version);
  return [...cells.values()].find((c) => c.recommended)?.result;
}

export function formatDate(iso: string | null | undefined): string {
  if (!iso) return 'not yet';
  return new Date(iso).toLocaleDateString('en-GB', { day: 'numeric', month: 'long', year: 'numeric' });
}

export function formatDuration(seconds: number): string {
  if (seconds < 60) return `${Math.round(seconds)}s`;
  return `${Math.floor(seconds / 60)}m ${String(Math.round(seconds % 60)).padStart(2, '0')}s`;
}

export function resetCache(): void {
  _dimensions = null;
  _cells.clear();
}
