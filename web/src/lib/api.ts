import { activity, rememberOperation } from './operations.svelte'

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
  netnsOf?: string // the container whose network namespace this one joins
  netnsUnit?: string // its unit, when a Quadlet here defines it
  sharedBy?: string[] // units of the containers that join this one's network namespace
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
  state: 'running' | 'partial' | 'stopped' | 'failed' | 'empty' | 'busy' | 'unknown'
}

export type OperationStep = {
  name: string
  unit?: string
  state: string
  detail?: string
  error?: string
  started?: string
  finished?: string
}

export type Operation = {
  id: string
  label: string
  stack?: string
  state: string
  created: string
  revision: number
  deadline: string
  started?: string
  finished?: string
  error?: string
  steps: OperationStep[]
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
  lastFrom?: string
  rules: Rules
}

export type AuditEntry = { time: string; grant: string; label: string; from?: string; tool: string; args?: unknown; result: 'started' | 'ok' | 'denied' | 'error'; error?: string }
export type DeletedFile = { path: string; versions: number; time: string }

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
  let res: Response
  try {
    res = await fetch(url, {
      method,
      headers: body === undefined ? { 'X-Quadstack': '1' } : { 'X-Quadstack': '1', 'Content-Type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: AbortSignal.timeout(url.startsWith('/api/operations') ? 5000 : 30000),
    })
  } catch (error) {
    const reason = error instanceof Error ? error.message : String(error)
    throw new ApiError(`${reason}.${method === 'GET' ? '' : ' The request may already have been accepted. Check Activity before retrying.'}`, 0, null)
  }
  if (res.status === 401) {
    location.href = '/auth/login?next=' + encodeURIComponent(location.pathname + location.hash)
    throw new ApiError('Signed out', 401, null)
  }
  const data = await res.json().catch(() => undefined)
  if (!res.ok) throw new ApiError(data?.error ?? res.statusText, res.status, data)
  if (data === undefined) throw new ApiError(`Unexpected response from ${method} ${url}`, res.status, null)
  if (data.operation) rememberOperation(data.operation)
  return data as T
}

async function tracked(method: string, url: string, body?: unknown): Promise<Operation> {
  let { operation } = await call<{ operation: Operation }>(method, url, body)
  while (operation.state === 'queued' || operation.state === 'running') {
    await new Promise((resolve) => setTimeout(resolve, 1000))
    try {
      operation = await call<Operation>('GET', '/api/operations/' + operation.id)
      rememberOperation(operation)
    } catch (error) {
      throw new ApiError(`Could not confirm the action's outcome. It may still be running; check Activity before retrying. ${error instanceof Error ? error.message : error}`, 0, { operation })
    }
  }
  if (operation.state !== 'succeeded') throw new ApiError(operation.error || `Action ${operation.state}`, 502, { operation })
  return operation
}

const q = (params: Record<string, string>) => new URLSearchParams(params).toString()

export type Apply = '' | 'reload' | 'restart'

export const api = {
  me: () => call<Me>('GET', '/api/me'),
  operations: () => call<Operation[]>('GET', '/api/operations'),
  stacks: () => call<{ stacks: Stack[]; resources: Resource[]; warnings: string[] | null }>('GET', '/api/stacks'),
  createStack: (name: string) => call<{ id: string }>('POST', '/api/stacks', { name }),
  removeStack: (id: string) => call('DELETE', '/api/stacks/' + id),
  stackAction: (stack: string, action: string) => tracked('POST', '/api/stack-action', { stack, action }),
  unitAction: (unit: string, action: string) => tracked('POST', '/api/unit-action', { unit, action }),
  reload: () => tracked('POST', '/api/reload'),
  pull: (stack: string) => tracked('POST', '/api/pull', { stack }),
  readFile: (path: string) => call<{ content: string }>('GET', '/api/file?' + q({ path })),
  saveFile: (path: string, content: string, apply: Apply, force = false) =>
    call<{ validation: Validation; applyError?: string; operation?: Operation }>('PUT', '/api/file', { path, content, apply, force }),
  createFile: async (path: string, content: string) => {
    const result = await call<{ applyError?: string }>('POST', '/api/file', { path, content, apply: 'reload' })
    if (result.applyError) activity.warning = result.applyError
    return result
  },
  deleteFile: (path: string, stop: boolean) => tracked('DELETE', '/api/file?' + q({ path, stop: stop ? '1' : '0' })),
  moveFile: (from: string, to: string) => call<{ applyError?: string }>('POST', '/api/file/move', { from, to }),
  validate: (path: string, content: string) => call<Validation>('POST', '/api/validate', { path, content }),
  history: (path: string) => call<Version[]>('GET', '/api/history?' + q({ path })),
  version: (path: string, id: string) => call<{ content: string }>('GET', '/api/history/version?' + q({ path, id })),
  deletedFiles: () => call<DeletedFile[]>('GET', '/api/history/deleted'),
  forgetHistory: (path: string) => call('DELETE', '/api/history?' + q({ path })),
  // Recreates a deleted file from a saved version; force saves it although validation fails
  restoreFile: (path: string, content: string, force = false) =>
    call<{ applyError?: string }>('POST', '/api/file', { path, content, apply: 'reload', force }),
  images: () => call<Image[]>('GET', '/api/images'),
  prune: async (all: boolean) => ({ output: (await tracked('POST', '/api/images/prune', { all })).steps[0]?.detail ?? '' }),
  podmanResources: () => call<{ networks: PodmanNetwork[] | null; volumes: PodmanVolume[] | null }>('GET', '/api/podman-resources'),
  secrets: () => call<SecretInfo[]>('GET', '/api/secrets'),
  setSecret: (s: { name: string; value?: string; fromFile?: string; replace?: boolean }) => tracked('POST', '/api/secrets', s),
  removeSecret: (name: string, force = false) => tracked('DELETE', '/api/secrets?' + q({ name, force: force ? '1' : '0' })),
  mcp: () => call<McpInfo>('GET', '/api/mcp'),
  createGrant: (g: { label: string; minutes: number; rules: Rules }) => call<{ grant: Grant; token: string }>('POST', '/api/mcp/grants', g),
  updateGrant: (id: string, g: { label: string; minutes: number; rules: Rules }) => call<Grant>('PUT', '/api/mcp/grants/' + id, g),
  endGrant: (id: string) => call('POST', '/api/mcp/grants/' + id + '/end'),
  deleteGrant: (id: string) => call('DELETE', '/api/mcp/grants/' + id),
  convert: (input: string) => call<{ output: string }>('POST', '/api/convert', { input }),
}

export function wsURL(path: string, params: Record<string, string | string[]>) {
  const u = new URL(path, location.href)
  u.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:'
  for (const [k, v] of Object.entries(params)) for (const x of [v].flat()) u.searchParams.append(k, x)
  return u.toString()
}
