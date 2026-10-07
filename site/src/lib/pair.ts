import { getProductNames } from './matrix.js';
import {
  getCells,
  getDimensions,
  getLines,
  PRODUCT_LABELS,
  STEP_LABELS,
  type Cell,
  type Dimension,
  type ServiceOption,
} from './compat.js';
import { answerFor } from './service.js';
import { neededWorkarounds } from './workarounds.js';

export type Verdict = 'yes' | 'partly' | 'no' | 'untested';

export interface PairFailure {
  summary: string;
  step: string;
  versions: string[];
}

export interface SiblingPair {
  option: ServiceOption;
  passing: number;
  tested: number;
  href: string;
}

export interface LinePair {
  line: string;
  passing: number;
  tested: number;
  href: string;
}

/** One release line (Magento 2.4.8, Mage-OS 3.x) against one service version (PHP 8.4). */
export interface Pair {
  product: string;
  label: string;
  line: string;
  option: ServiceOption;
  dim: Dimension;
  href: string;
  /** Oldest first, so the strip reads base, p1, p2 ... */
  releases: Array<{ version: string; cell: Cell }>;
  newest: string;
  passing: number;
  failing: number;
  tested: number;
  verdict: Verdict;
  lastRun: string | null;
  /** What the vendor's system requirements list for the newest release of the line. */
  recommended?: ServiceOption;
  failures: PairFailure[];
  /** Titles of the fixes the passing runs needed, when the release as shipped is not enough. */
  fixes: string[];
  /** The newest release of the product that installs on this option, when it is in another line. */
  elsewhere?: string;
  siblings: SiblingPair[];
  otherLines: LinePair[];
}

export function pairHref(product: string, line: string, option: ServiceOption): string {
  return `/${product}/${line}/${option.type}-${option.version}/`;
}

export function pairPaths() {
  return getProductNames().flatMap((product) =>
    getLines(product).flatMap(({ line }) =>
      getDimensions().flatMap((d) =>
        d.options.map((o) => ({ params: { product, line, service: `${o.type}-${o.version}` }, props: { key: d.key } })),
      ),
    ),
  );
}

function tally(product: string, releases: string[], option: ServiceOption) {
  const cells = releases.map((v) => getCells(product, v).get(option.id)!);
  const passing = cells.filter((c) => c.status === 'pass').length;
  return { passing, tested: cells.filter((c) => c.status !== 'untested').length };
}

export function getPair(product: string, line: string, option: ServiceOption): Pair | undefined {
  const found = getLines(product).find((l) => l.line === line);
  if (!found) return undefined;
  const dim = getDimensions().find((d) => d.key === option.key)!;
  const label = PRODUCT_LABELS[product] ?? product;
  const releases = found.releases.map((r) => ({ version: r.version, cell: getCells(product, r.version).get(option.id)! }));
  const tested = releases.filter((r) => r.cell.status !== 'untested');
  const passing = tested.filter((r) => r.cell.status === 'pass');
  const failing = tested.filter((r) => r.cell.status === 'fail');
  const newest = releases.at(-1)!.version;

  const failures = new Map<string, PairFailure>();
  for (const r of failing) {
    const f = r.cell.failure;
    if (!f) continue;
    const g = failures.get(f.code) ?? { summary: f.summary, step: STEP_LABELS[f.step], versions: [] };
    g.versions.push(r.version);
    failures.set(f.code, g);
  }

  const fixes = [...new Set(passing.flatMap((r) => neededWorkarounds(r.cell.result?.workarounds).map((w) => w.title)))];
  const lastRun = tested.map((r) => r.cell.result!.timestamp).sort().at(-1) ?? null;
  const recommended = [...getCells(product, newest).values()].find((c) => c.recommended && c.option.key === option.key)?.option;
  const elsewhereNewest = answerFor(product, option).newest;
  const elsewhere = elsewhereNewest && !releases.some((r) => r.version === elsewhereNewest) ? elsewhereNewest : undefined;

  return {
    product,
    label,
    line,
    option,
    dim,
    href: pairHref(product, line, option),
    releases,
    newest,
    passing: passing.length,
    failing: failing.length,
    tested: tested.length,
    verdict: tested.length === 0 ? 'untested' : failing.length === 0 ? 'yes' : passing.length === 0 ? 'no' : 'partly',
    lastRun,
    recommended,
    failures: [...failures.values()].sort((a, b) => b.versions.length - a.versions.length),
    fixes,
    elsewhere,
    siblings: dim.options.map((o) => ({ option: o, ...tally(product, found.releases.map((r) => r.version), o), href: pairHref(product, line, o) })),
    otherLines: getLines(product)
      .filter((l) => l.line !== line)
      .map((l) => ({ line: l.line, ...tally(product, l.releases.map((r) => r.version), option), href: pairHref(product, l.line, option) })),
  };
}

export function pairQuestion(p: Pair): string {
  return `Does ${p.label} ${p.line} run on ${p.option.label}?`;
}

const list = (xs: string[]) => (xs.length < 2 ? xs.join('') : `${xs.slice(0, -1).join(', ')} and ${xs.at(-1)}`);
const plural = (n: number, word: string) => `${n} ${word}${n === 1 ? '' : 's'}`;

/** The plain-English verdict: the first thing on the page, the meta description and the FAQ answer. */
export function pairAnswer(p: Pair): string {
  const source = p.product === 'magento' ? 'Adobe' : 'Mage-OS';
  const subject = `${p.label} ${p.line}`;
  const reason = p.failures[0] ? ` ${p.failures[0].summary}` : '';
  const listed = p.recommended ? ` ${source} lists ${p.recommended.label} for ${p.newest}.` : '';
  const elsewhere = p.elsewhere ? ` The newest ${p.label} release that installs on ${p.option.label} is ${p.elsewhere}.` : '';

  switch (p.verdict) {
    case 'untested':
      return `Not tested yet. We have no run of ${subject} on ${p.option.label}.${listed}`;
    case 'yes': {
      const fixes = p.fixes.length ? ` Some runs needed an extra step: ${list(p.fixes)}.` : '';
      const every = p.tested === 1 ? `The ${subject} release we tested installs` : `All ${p.tested} ${subject} releases we tested install`;
      return `Yes. ${every} on ${p.option.label} and pass the storefront, checkout and admin tests.${fixes}${listed}`;
    }
    case 'no':
      return `No. None of the ${plural(p.tested, `${subject} release`)} we tested install on ${p.option.label}.${reason}${elsewhere}${listed}`;
    case 'partly': {
      const passed = p.releases.filter((r) => r.cell.status === 'pass').map((r) => r.version);
      const failed = p.releases.filter((r) => r.cell.status === 'fail').map((r) => r.version);
      return `Partly. ${p.passing} of ${p.tested} ${subject} releases install on ${p.option.label}: ${list(passed)}. ${list(failed)} fail${failed.length === 1 ? 's' : ''}.${reason}${listed}`;
    }
  }
}
