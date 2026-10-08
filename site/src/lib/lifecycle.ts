import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { load } from 'js-yaml';
import releaseDates from '../data/release-dates.json';
import { formatDate, getLines, PRODUCT_LABELS } from './compat.js';

export const CREDITS = {
  magentoWatch: { name: 'magento.watch', url: 'https://magento.watch/', author: 'Lukasz Bajsarowicz', authorUrl: 'https://github.com/lbajsarowicz' },
  mageOSReleases: { name: 'Mage-OS releases on GitHub', url: 'https://github.com/mage-os/mageos-magento2/releases' },
};

interface AdobeSupport {
  policy_url: string;
  policy_updated: string;
  lines: Record<string, { standard: string; extended?: string; security_only?: string }>;
}

const adobe = load(readFileSync(resolve(process.cwd(), 'src/data/adobe-support.yml'), 'utf-8')) as AdobeSupport;
export const ADOBE_POLICY = { url: adobe.policy_url, updated: adobe.policy_updated };

export type Phase = 'standard' | 'extended' | 'security' | 'ended';

export interface Support {
  line: string;
  released?: string;
  standard: string;
  extended?: string;
  securityOnly?: string;
  phase: Phase;
  /** When the current phase ends, or when support ended for a line past all of them. */
  until: string;
}

const today = new Date().toISOString().slice(0, 10);

export function releasedOn(product: string, version: string): string | undefined {
  return (releaseDates as Record<string, Record<string, string>>)[product]?.[version];
}

/** Adobe's support dates for a Magento line, with where it stands on the day the site was built. */
export function supportFor(product: string, line: string): Support | undefined {
  const s = product === 'magento' ? adobe.lines[line] : undefined;
  if (!s) return undefined;
  const steps: Array<[Phase, string | undefined]> = [
    ['standard', s.standard],
    ['extended', s.extended],
    ['security', s.security_only],
  ];
  const current = steps.find(([, end]) => end && today <= end);
  const last = steps.filter(([, end]) => end).at(-1)![1]!;
  return {
    line,
    released: releasedOn(product, line),
    standard: s.standard,
    extended: s.extended,
    securityOnly: s.security_only,
    phase: current ? current[0] : 'ended',
    until: current ? current[1]! : last,
  };
}

export const PHASE_LABELS: Record<Phase, string> = {
  standard: 'Supported',
  extended: 'Extended support',
  security: 'Security fixes only',
  ended: 'End of life',
};

/** One sentence on where a line stands, e.g. "2.4.6 is in extended support until 31 August 2027." */
export function supportSentence(s: Support): string {
  const label = `${PRODUCT_LABELS.magento} ${s.line}`;
  switch (s.phase) {
    case 'standard':
      return `${label} is in Adobe's standard support, with quality and security patches, until ${formatDate(s.until)}.`;
    case 'extended':
      return `${label} left standard support on ${formatDate(s.standard)}. Extended support, with quality and security patches for Adobe Commerce customers, runs until ${formatDate(s.until)}.`;
    case 'security':
      return `${label} gets security fixes only, until ${formatDate(s.until)}.`;
    case 'ended':
      return `${label} reached end of life on ${formatDate(s.until)}. Adobe no longer ships patches for it.`;
  }
}

/** Every Magento line Adobe has dates for, newest first. */
export function getSupportTable(product: string): Array<Support & { newest: string; newestReleased?: string }> {
  return getLines(product).flatMap(({ line, releases }) => {
    const s = supportFor(product, line);
    const newest = releases.at(-1)!.version;
    return s ? [{ ...s, newest, newestReleased: releasedOn(product, newest) }] : [];
  });
}

export const buildDate = today;
