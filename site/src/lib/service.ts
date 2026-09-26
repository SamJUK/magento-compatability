import { getProductNames } from './matrix.js';
import { getCells, getDimensions, getLines, getReleases, type ServiceOption } from './compat.js';

export interface ProductAnswer {
  product: string;
  newest?: string;
  oldest?: string;
  passing: number;
  /** The first release newer than `newest`, and what it installs on instead. */
  next?: { version: string; status: 'fail' | 'untested'; worksOn: ServiceOption[] };
}

export function answerFor(product: string, option: ServiceOption): ProductAnswer {
  const releases = getReleases(product);
  const passes = (v: string) => getCells(product, v).get(option.id)?.status === 'pass';
  const idx = releases.findIndex((r) => passes(r.version));
  const passing = releases.filter((r) => passes(r.version));
  const answer: ProductAnswer = {
    product,
    newest: releases[idx]?.version,
    oldest: passing.at(-1)?.version,
    passing: passing.length,
  };
  if (idx > 0) {
    const next = releases[idx - 1];
    const cells = getCells(product, next.version);
    const dim = getDimensions().find((d) => d.key === option.key)!;
    answer.next = {
      version: next.version,
      status: cells.get(option.id)?.status === 'fail' ? 'fail' : 'untested',
      worksOn: dim.options.filter((o) => cells.get(o.id)?.status === 'pass'),
    };
  }
  return answer;
}

export function stripsFor(product: string, option: ServiceOption) {
  return getLines(product).map(({ line, releases }) => {
    const cells = releases.map((r) => ({ version: r.version, cell: getCells(product, r.version).get(option.id)! }));
    return { line, cells, passing: cells.filter((c) => c.cell.status === 'pass').length };
  });
}

export function newestPerProduct(option: ServiceOption): Array<{ product: string; newest?: string }> {
  return getProductNames().map((product) => ({ product, newest: answerFor(product, option).newest }));
}
