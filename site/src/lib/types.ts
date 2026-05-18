// ─── Status types ────────────────────────────────────────────────────────────

export type ServiceStatus = 'pass' | 'fail' | 'partial' | 'unknown';

export type StepStatus = 'pass' | 'fail' | 'skip' | 'error';

// ─── Raw result JSON schema (mirrors results/*.json files) ───────────────────

export interface StepResult {
  status: StepStatus;
  duration_s: number;
  log: string;
}

export interface ServiceInfo {
  type: string;
  version: string;
}

export interface TestServices {
  php: string;
  webserver: string;
  db: ServiceInfo;
  search: ServiceInfo;
  cache: ServiceInfo;
  queue: ServiceInfo;
  varnish: string;
}

export interface TestResult {
  id: string;
  product: string;
  version: string;
  overall_status: 'pass' | 'fail';
  services: TestServices;
  steps: {
    stack_up: StepResult;
    install: StepResult;
    smoke: StepResult;
    playwright: StepResult;
  };
  /** Per-service container logs captured after the run, keyed by service name */
  container_logs?: Record<string, string>;
  timestamp: string;
}

// A synthetic placeholder for an untested combination
export interface UntestedResult {
  id: string;
  product: string;
  version: string;
  overall_status: 'unknown';
  services: TestServices;
  steps: null;
  timestamp: null;
}

export type AnyResult = TestResult | UntestedResult;

// ─── Matrix definition (mirrors matrix.yml) ──────────────────────────────────

export interface Baseline {
  php: string;
  webserver: string;
  db: ServiceDefinition;
  search: ServiceDefinition;
  cache: ServiceDefinition;
  queue: ServiceDefinition;
  varnish: string;
}

export interface ProductVersion {
  version: string;
  baseline?: Baseline;
}

export interface ProductDefinition {
  name: string;
  package: string;
  mirror: string;
  versions: ProductVersion[];
}

export interface WebserverDefinition {
  type: string;
  version: string;
}

export interface ServiceDefinition {
  type: string;
  version: string;
}

export interface MatrixServices {
  php: string[];
  webserver: WebserverDefinition[];
  database: ServiceDefinition[];
  search: ServiceDefinition[];
  cache: ServiceDefinition[];
  queue: ServiceDefinition[];
  varnish: string[];
}

export interface MatrixDefinition {
  products: ProductDefinition[];
  services: MatrixServices;
}

// ─── Aggregated status for a service version across multiple results ──────────

export interface AggregatedServiceStatus {
  /** Aggregate status: pass (all pass), partial (some pass), fail (all fail), unknown (none tested) */
  status: ServiceStatus;
  passed: number;
  failed: number;
  total: number;
  /** IDs of underlying result files */
  resultIds: string[];
}

// ─── Summary views ───────────────────────────────────────────────────────────

export interface ServiceVersionEntry {
  key: string;
  type: string;
  version: string;
  label: string;
  aggregate: AggregatedServiceStatus;
}

export interface ServiceRowGroup {
  /** e.g. "PHP", "Database", "Search" */
  label: string;
  /** e.g. "php", "db", "search" */
  key: string;
  entries: ServiceVersionEntry[];
}

export interface VersionSummary {
  product: string;
  version: string;
  passCount: number;
  failCount: number;
  unknownCount: number;
  totalCombinations: number;
  serviceRows: ServiceRowGroup[];
  lastTested: string | null;
}

export interface SoftwareVersionSummary {
  serviceType: string;
  serviceVersion: string;
  label: string;
  /** Keyed by `${product}/${version}` */
  compatibility: Record<string, {
    product: string;
    version: string;
    aggregate: AggregatedServiceStatus;
  }>;
}
