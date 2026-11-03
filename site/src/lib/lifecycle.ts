/**
 * Copyright (c) 2026 Sam James. Licensed under the Open Software License version 3.0.
 * SPDX-License-Identifier: OSL-3.0
 */
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { load } from 'js-yaml';
import releaseDates from '../data/release-dates.json';

interface AdobeSupport {
  policy_url: string;
  lines: Record<string, { standard: string; extended?: string; security_only?: string }>;
}

const adobe = load(readFileSync(resolve(process.cwd(), 'src/data/adobe-support.yml'), 'utf-8')) as AdobeSupport;
export const ADOBE_POLICY_URL = adobe.policy_url;

export type Phase = 'standard' | 'extended' | 'security' | 'ended';

export interface Support {
  line: string;
  phase: Phase;
  /** When the current phase ends, or when support ended for a line past all of them. */
  until: string;
}

const today = new Date().toISOString().slice(0, 10);

export function releasedOn(product: string, version: string): string | undefined {
  return (releaseDates as Record<string, Record<string, string>>)[product]?.[version];
}

/** Where a Magento line stands in Adobe's lifecycle on the day the site was built. */
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
  return { line, phase: current ? current[0] : 'ended', until: current ? current[1]! : last };
}
