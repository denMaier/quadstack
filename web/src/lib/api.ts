export type UnitState = { load: string; active: string; sub: string }

export type QFile = {
  path: string
  kind: string
  unit?: string
  pod?: string
  image?: string
  containerName?: string
  autostart: boolean
  parseError?: string
}

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
}

export type Validation = { ok: boolean; skipped?: string; output: string; generated: string }

export type Version = { id: string; time: string; size: number }

export type Image = { Id: string; Names: string[] | null; Size: number; Created: number; Containers: number; Dangling: boolean }

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
  const data = await res.json().catch(() => null)
  if (!res.ok) throw new ApiError(data?.error ?? res.statusText, res.status, data)
  return data as T
}

const q = (params: Record<string, string>) => new URLSearchParams(params).toString()

export type Apply = '' | 'reload' | 'restart'

export const api = {
  me: () => call<Me>('GET', '/api/me'),
  stacks: () => call<{ stacks: Stack[]; warnings: string[] | null }>('GET', '/api/stacks'),
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
  convert: (input: string) => call<{ output: string }>('POST', '/api/convert', { input }),
}

export function wsURL(path: string, params: Record<string, string | string[]>) {
  const u = new URL(path, location.href)
  u.protocol = location.protocol === 'https:' ? 'wss:' : 'ws:'
  for (const [k, v] of Object.entries(params)) for (const x of [v].flat()) u.searchParams.append(k, x)
  return u.toString()
}
