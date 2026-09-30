import { existsSync, readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import type { TestResult, Workaround } from './types.js';

const REGISTRY = resolve(process.cwd(), '../docker/scripts/workarounds.json');

let _registry: Map<string, Workaround> | null = null;

function registry(): Map<string, Workaround> {
  if (!_registry) {
    const list: Workaround[] = existsSync(REGISTRY) ? JSON.parse(readFileSync(REGISTRY, 'utf-8')).workarounds : [];
    _registry = new Map(list.map((w) => [w.id, w]));
  }
  return _registry;
}

export function getWorkaround(id: string): Workaround {
  return registry().get(id) ?? { id, kind: 'patch', title: id, detail: '', product: '', versions: [] };
}

/** Workarounds a user would need too, leaving out ones only our test environment needs. */
export function neededWorkarounds(ids: string[] | undefined): Workaround[] {
  return (ids ?? []).map(getWorkaround).filter((w) => !w.environment);
}

export interface WorkaroundUsage extends Workaround {
  used: number;
  passed: number;
}

export function workaroundUsage(results: TestResult[]): WorkaroundUsage[] {
  const usage = new Map<string, WorkaroundUsage>();
  for (const r of results) {
    for (const id of r.workarounds ?? []) {
      const u = usage.get(id) ?? { ...getWorkaround(id), used: 0, passed: 0 };
      u.used++;
      if (r.overall_status === 'pass') u.passed++;
      usage.set(id, u);
    }
  }
  return [...usage.values()].sort((a, b) => Number(!!a.environment) - Number(!!b.environment) || b.used - a.used);
}
