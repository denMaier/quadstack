export type UnitState = { load: string; active: string; sub: string }

export type QFile = {
  path: string
  kind: string
  unit?: string
  pod?: string
  image?: string
  containerName?: string
  name?: string
  autostart: boolean
  parseError?: string
  networks?: string[]
  mounts?: Mount[]
  secrets?: SecretRef[]
}

export type Mount = { kind: string; source?: string; target: string; options?: string }

export type SecretRef = { name: string; type: string; target: string }

export type Resource = { file: QFile; stack: string; usedBy: string[]; state: UnitState }

export type SecretInfo = {
  name: string
  driver?: string
  created?: string
  updated?: string
  exists: boolean
  usedBy: { stack: string; path: string; type: string; target: string }[]
}

export type PodmanNetwork = { name: string; driver: string; internal: boolean; dns_enabled: boolean; subnets: { subnet: string; gateway: string }[] | null }

export type PodmanVolume = { Name: string; Driver: string; Mountpoint: string; CreatedAt: string }

export type Container = {
  Id: string
  Names: string[]
  Image: string
  State: string
  Status: string
  PodName: string
  Labels: Record<string, string> | null
}

export type Stack = {
  id: string
  name: string
  dir: string
  files: QFile[]
  units: Record<string, UnitState>
  containers: Container[]
  state: 'running' | 'partial' | 'stopped' | 'failed' | 'empty'
}

export type Me = {
  name: string
  host: string
  quadletDir: string
  validation: boolean
  podlet: boolean
  terminal: boolean
  journal: string
  version: string
}

export type Validation = { ok: boolean; skipped?: string; output: string; generated: string }

export type Version = { id: string; time: string; size: number }

export type Image = { Id: string; Names: string[] | null; Size: number; Created: number; Containers: number; Dangling: boolean }

export type Perm = { id: string; group: string; label: string; help: string; scoped: boolean }

export type Rules = Record<string, string[]>

export type Grant = {
  id: string
  label: string
  createdBy: string
  created: string
  expires: string
  revoked?: string
  lastUsed?: string
  rules: Rules
}

export type AuditEntry = { time: string; grant: string; label: string; tool: string; args?: unknown; result: 'ok' | 'denied' | 'error'; error?: string }

export type McpInfo = { enabled: boolean; maxMinutes: number; endpoint: string; serverName: string; permissions: Perm[]; grants: Grant[]; audit: AuditEntry[] }

export class ApiError extends Error {
  constructor(
    message: string,
    public status: number,
    public body: any,
  ) {
    super(message)
  }
}

async function call<T>(method: string, url: string, body?: unknown): Promise<T> {
  const res = await fetch(url, {
    method,
    headers: body === undefined ? { 'X-Quadstack': '1' } : { 'X-Quadstack': '1', 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  if (res.status === 401) {
    location.href = '/auth/login?next=' + encodeURIComponent(location.pathname + location.hash)
    throw new ApiError('Signed out', 401, null)
  }
  const data = await res.json().catch(() => undefined)
  if (!res.ok) throw new ApiError(data?.error ?? res.statusText, res.status, data)
  if (data === undefined) throw new ApiError(`Unexpected response from ${method} ${url}`, res.status, null)
  return data as T
}

const q = (params: Record<string, string>) => new URLSearchParams(params).toString()

export type Apply = '' | 'reload' | 'restart'

export const api = {
  me: () => call<Me>('GET', '/api/me'),
  stacks: () => call<{ stacks: Stack[]; resources: Resource[]; warnings: string[] | null }>('GET', '/api/stacks'),
  createStack: (name: string) => call<{ id: string }>('POST', '/api/stacks', { name }),
  removeStack: (id: string) => call('DELETE', '/api/stacks/' + id),
  stackAction: (stack: string, action: string) => call('POST', '/api/stack-action', { stack, action }),
  unitAction: (unit: string, action: string) => call('POST', '/api/unit-action', { unit, action }),
  reload: () => call('POST', '/api/reload'),
  readFile: (path: string) => call<{ content: string }>('GET', '/api/file?' + q({ path })),
  saveFile: (path: string, content: string, apply: Apply, force = false) =>
    call<{ validation: Validation; applyError?: string }>('PUT', '/api/file', { path, content, apply, force }),
  createFile: (path: string, content: string) => call('POST', '/api/file', { path, content, apply: 'reload' }),
  deleteFile: (path: string, stop: boolean) => call('DELETE', '/api/file?' + q({ path, stop: stop ? '1' : '0' })),
  moveFile: (from: string, to: string) => call('POST', '/api/file/move', { from, to }),
  validate: (path: string, content: string) => call<Validation>('POST', '/api/validate', { path, content }),
  history: (path: string) => call<Version[]>('GET', '/api/history?' + q({ path })),
  version: (path: string, id: string) => call<{ content: string }>('GET', '/api/history/version?' + q({ path, id })),
  images: () => call<Image[]>('GET', '/api/images'),
  prune: (all: boolean) => call<{ output: string }>('POST', '/api/images/prune', { all }),
  podmanResources: () => call<{ networks: PodmanNetwork[] | null; volumes: PodmanVolume[] | null }>('GET', '/api/podman-resources'),
  secrets: () => call<SecretInfo[]>('GET', '/api/secrets'),
  setSecret: (s: { name: string; value?: string; fromFile?: string; replace?: boolean }) => call('POST', '/api/secrets', s),
  removeSecret: (name: string, force = false) => call('DELETE', '/api/secrets?' + q({ name, force: force ? '1' : '0' })),
  mcp: () => call<McpInfo>('GET', '/api/mcp'),
  createGrant: (g: { label: string; minutes: number; rules: Rules }) => call<{ grant: Grant; token: string }>('POST', '/api/mcp/grants', g),
  updateGrant: (id: string, g: { label: string; minutes: number; rules: Rules }) => call<Grant>('PUT', '/api/mcp/grants/' + id, g),
  revokeGrant: (id: string) => call('DELETE', '/api/mcp/grants/' + id),
  convert: (input: string) => call<{ output: string }>('POST', '/api/convert', { input }),
}

export function wsURL(path: string, params: Record<string, string | string[]>) {
  const u = new URL(path, location.href)
  u.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:'
  for (const [k, v] of Object.entries(params)) for (const x of [v].flat()) u.searchParams.append(k, x)
  return u.toString()
}
