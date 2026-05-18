import type { APIRoute, GetStaticPaths } from 'astro';
import { getAllServicePaths, getSoftwareVersionSummary } from '@lib/results.js';

export const getStaticPaths: GetStaticPaths = () => {
  return getAllServicePaths().map((entry) => ({
    params: { service: entry.key, version: `${entry.type}-${entry.version}` },
    props: { key: entry.key, type: entry.type, version: entry.version, label: entry.label },
  }));
};

export const GET: APIRoute = ({ props }) => {
  const { key, type, version, label } = props as { key: string; type: string; version: string; label: string };
  const summary = getSoftwareVersionSummary(key, type, version);
  return new Response(JSON.stringify({ key, type, version, label, ...summary }, null, 2), {
    headers: { 'Content-Type': 'application/json' },
  });
};
