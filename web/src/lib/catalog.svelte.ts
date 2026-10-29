import { api, type Resource, type SecretInfo } from './api'

// Shared lists the editor completes from and several views show
export const catalog = $state({
  resources: [] as Resource[],
  secrets: [] as SecretInfo[],
})

export async function loadSecrets() {
  try {
    catalog.secrets = await api.secrets()
  } catch {
    // the Secrets page shows the error; completion just stays empty
  }
}
