/**
 * Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
 * SPDX-License-Identifier: OSL-3.0
 */

import { getProductNames } from './matrix.js';
import { dimTypeNames, getDimensions, getLines, PRODUCT_LABELS, type DimKey, type Dimension } from './compat.js';
import { getPair, type Pair } from './pair.js';

/** The services people pick a version of. Web server and Varnish have two or three options and no line pages. */
export const HUB_DIMS: DimKey[] = ['php', 'db', 'search', 'cache', 'queue'];

/** Every version of one service against one release line: /magento/2.4.6/search/ */
export interface LineHub {
  product: string;
  label: string;
  line: string;
  dim: Dimension;
  href: string;
  pairs: Pair[];
  newest: string;
  lastRun: string | null;
}

export function lineHubHref(product: string, line: string, key: DimKey): string | undefined {
  return HUB_DIMS.includes(key) ? `/${product}/${line}/${key}/` : undefined;
}

export function lineHubPaths() {
  return getProductNames().flatMap((product) =>
    getLines(product).flatMap(({ line }) => HUB_DIMS.map((key) => ({ params: { product, line, service: key }, props: { key, hub: true } }))),
  );
}

export function getLineHub(product: string, line: string, key: DimKey): LineHub {
  const dim = getDimensions().find((d) => d.key === key)!;
  const pairs = dim.options.map((o) => getPair(product, line, o)!);
  return {
    product,
    label: PRODUCT_LABELS[product] ?? product,
    line,
    dim,
    href: lineHubHref(product, line, key)!,
    pairs,
    newest: pairs[0].newest,
    lastRun: pairs.map((p) => p.lastRun).filter(Boolean).sort().at(-1) ?? null,
  };
}

export function hubQuestion(h: LineHub): string {
  return `Which ${dimTypeNames(h.dim)} versions does ${h.label} ${h.line} support?`;
}

const list = (xs: string[], last = 'and') => (xs.length < 2 ? xs.join('') : `${xs.slice(0, -1).join(', ')} ${last} ${xs.at(-1)}`);

/** "Elasticsearch 7.10.1 and 8.11.4, and OpenSearch 2.19.5": versions grouped under their service. */
function grouped(pairs: Pair[], last = 'and'): string {
  const groups = new Map<string, string[]>();
  for (const { option: o } of pairs) {
    const name = o.label.slice(0, -o.version.length).trim();
    groups.set(name, [...(groups.get(name) ?? []), o.version]);
  }
  const parts = [...groups].map(([name, versions]) => `${name} ${list(versions, last)}`);
  return parts.length < 2 ? parts.join('') : `${parts.slice(0, -1).join(', ')}, ${last} ${parts.at(-1)}`;
}

/** "from 2.4.6-p4", "up to 2.4.6-p3", "on 2.4.6 only", or "on 5 of 16 releases": which releases of a line pass. */
function passingSpan(p: Pair): string {
  const tested = p.releases.filter((r) => r.cell.status !== 'untested');
  const pass = tested.map((r) => r.cell.status === 'pass');
  const first = pass.indexOf(true);
  const last = pass.lastIndexOf(true);
  const contiguous = pass.slice(first, last + 1).every(Boolean);
  if (first === last) return `on ${tested[first].version} only`;
  if (contiguous && last === pass.length - 1) return `from ${tested[first].version}`;
  if (contiguous && first === 0) return `up to ${tested[last].version}`;
  return `on ${p.passing} of ${p.tested} releases`;
}

/** The plain-English answer: the meta description, the FAQ answer and the box at the top of the page. */
export function hubAnswer(h: LineHub): string {
  const subject = `${h.label} ${h.line}`;
  const by = (v: Pair['verdict']) => h.pairs.filter((p) => p.verdict === v);
  const yes = by('yes');
  const partly = by('partly').map((p) => `${p.option.label} works ${passingSpan(p)}`);
  const no = by('no');
  const fixed = h.pairs.filter((p) => p.fixes.length > 0);
  const fixes = [...new Set(fixed.flatMap((p) => p.fixes))];
  const parts = [
    yes.length ? `Every ${subject} release we tested installs on ${grouped(yes)}.` : `No ${dimTypeNames(h.dim)} version works on every ${subject} release we tested.`,
    partly.length ? `${list(partly)}.` : '',
    no.length ? `None install on ${grouped(no, 'or')}.` : '',
    fixed.length ? `On ${grouped(fixed)}, some runs needed an extra step: ${list(fixes)}.` : '',
  ];
  return parts.filter(Boolean).join(' ');
}
