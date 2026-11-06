import type { Operation } from './api'

export const activity = $state({ items: [] as Operation[], error: '', warning: '' })

export function isActive(operation: Operation) {
  return operation.state === 'queued' || operation.state === 'running'
}

export function rememberOperation(operation: Operation) {
  const index = activity.items.findIndex((item) => item.id === operation.id)
  if (index < 0) activity.items.unshift(operation)
  else if (activity.items[index].revision <= operation.revision) activity.items[index] = operation
}
