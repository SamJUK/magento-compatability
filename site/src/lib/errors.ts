/**
 * Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
 * SPDX-License-Identifier: OSL-3.0
 */

import { getProductNames } from './matrix.js';
import { getDimensions, getLines, PRODUCT_LABELS, type DimKey, type ServiceOption } from './compat.js';
import { getPair } from './pair.js';

/** An error Magento or Composer prints that our runs hit, keyed by the failure code the harness records. */
export interface KnownError {
  slug: string;
  code: string;
  dim: DimKey;
  /** As printed, for the title. */
  message: string;
  /** Finds the line in a run log... */
  needle: string;
  /** ...and the part of it to quote. */
  pattern: RegExp;
  /** What it means, from the code that raises it. Trusted HTML. */
  meaning: string;
}

export const KNOWN_ERRORS: KnownError[] = [
  {
    slug: 'current-version-of-rdbms-is-not-supported',
    code: 'db_version_unsupported',
    dim: 'db',
    message: 'Current version of RDBMS is not supported',
    needle: 'Current version of RDBMS is not supported',
    pattern: /Current version of RDBMS is not supported\..*/,
    meaning:
      "Magento compares the database server's version with the <code>supportedVersionPatterns</code> list in <code>app/etc/di.xml</code> (<code>Magento\\Framework\\DB\\Adapter\\SqlVersionProvider</code>) and stops when none match. The message ends with that list. Each release ships its own, so a version a newer release accepts can still be refused by an older one.",
  },
  {
    slug: 'your-php-version-does-not-satisfy-that-requirement',
    code: 'php_version_unsupported',
    dim: 'php',
    message: 'Your PHP version does not satisfy that requirement',
    needle: 'does not satisfy that requirement',
    pattern: /\S+ \S+ requires php .* does not satisfy that requirement\./,
    meaning:
      'Composer refuses to install the release. Its <code>product-community-edition</code> package lists the PHP versions it allows, and the PHP running Composer is not one of them. No Magento code runs, so nothing is installed.',
  },
  {
    slug: 'search-engine-is-not-an-available-search-engine',
    code: 'search_engine_unsupported',
    dim: 'search',
    message: "Search engine 'elasticsearch8' is not an available search engine",
    needle: 'is not an available search engine',
    pattern: /Search engine '[^']+' is not an available search engine\./,
    meaning:
      '<code>setup:install</code> checks <code>--search-engine</code> against the engines the release has modules for (<code>Magento\\Setup\\Model\\SearchConfig</code>) and stops before it connects to anything. Releases from before an engine\'s module refuse it, and releases that dropped an engine refuse it too.',
  },
  {
    slug: 'no-handler-found-for-uri-document-bulk',
    code: 'search_mapping_types_removed',
    dim: 'search',
    message: 'no handler found for uri [/magento2_product_1_v1/document/_bulk] and method [POST]',
    needle: 'no handler found for uri',
    pattern: /no handler found for uri \[[^\]]+\] and method \[POST\]/,
    meaning:
      'The release indexes through a <code>/{index}/document/_bulk</code> endpoint, which needs a mapping type the search engine no longer has. <code>setup:install</code> succeeds and the store loads. The error comes when a product is saved, so it can stay hidden until the first catalogue change.',
  },
  {
    slug: 'stomp-host-option-does-not-exist',
    code: 'stomp_unsupported',
    dim: 'queue',
    message: 'The "--stomp-host" option does not exist',
    needle: 'option does not exist',
    pattern: /The "--(?:stomp-host|queue-default-connection)" option does not exist\./,
    meaning:
      'The release has no <code>Magento_Stomp</code> module, so <code>setup:install</code> has no STOMP options and cannot connect to ActiveMQ Artemis. These releases need RabbitMQ.',
  },
];

export interface ErrorHit {
  label: string;
  option: ServiceOption;
  href: string;
  versions: string[];
  tested: number;
}

export interface ErrorReport {
  hits: ErrorHit[];
  runs: number;
  sample?: string;
  lastRun: string | null;
}

/** Symfony console prints errors in a padded box and cuts long lines mid-word: put the message back on one line. */
function unwrap(lines: string[], start: number): string {
  let text = lines[start].trim();
  for (let i = start + 1; i < lines.length && lines[i].trim() && /\s{2,}$/.test(lines[i - 1]); i++) {
    const cut = lines[i - 1].length - lines[i - 1].trimEnd().length === 2;
    text += (cut ? '' : ' ') + lines[i].trim();
  }
  return text;
}

function sampleFrom(log: string | undefined, e: KnownError): string | undefined {
  const lines = (log ?? '').split('\n');
  const at = lines.findIndex((l) => l.includes(e.needle));
  return at < 0 ? undefined : unwrap(lines, at).match(e.pattern)?.[0];
}

export function getErrorReport(e: KnownError): ErrorReport {
  const dim = getDimensions().find((d) => d.key === e.dim)!;
  const hits: ErrorHit[] = [];
  let sample: string | undefined;
  const stamps: string[] = [];
  for (const product of getProductNames()) {
    for (const { line } of getLines(product)) {
      for (const option of dim.options) {
        const pair = getPair(product, line, option)!;
        const failed = pair.releases.filter((r) => r.cell.failure?.code === e.code);
        if (!failed.length) continue;
        for (const { cell } of failed) {
          stamps.push(cell.result!.timestamp);
          sample ??= sampleFrom(cell.result?.steps?.[cell.failure!.step]?.log, e);
        }
        hits.push({ label: `${PRODUCT_LABELS[product] ?? product} ${line}`, option, href: pair.href, versions: failed.map((r) => r.version), tested: pair.tested });
      }
    }
  }
  return { hits, runs: hits.reduce((n, h) => n + h.versions.length, 0), sample, lastRun: stamps.sort().at(-1) ?? null };
}

export function errorHref(e: KnownError): string {
  return `/errors/${e.slug}/`;
}

export function errorForCode(code: string): KnownError | undefined {
  return KNOWN_ERRORS.find((e) => e.code === code);
}
