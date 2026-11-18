import { apiClient } from '../client'

export interface LarkQuotaSettings {
  enabled: boolean
  thresholds: number[]
  windows: ('5h' | '7d')[]
  webhook_configured: boolean
  signing_secret_configured: boolean
}

export interface LarkQuotaSettingsInput {
  enabled: boolean
  thresholds: number[]
  windows: ('5h' | '7d')[]
  webhook?: string
  signing_secret?: string
  clear_webhook?: boolean
  clear_signing_secret?: boolean
}

const path = '/admin/settings/lark-quota-alerts'
export async function getLarkQuotaSettings(): Promise<LarkQuotaSettings> {
  return (await apiClient.get<LarkQuotaSettings>(path)).data
}
export async function updateLarkQuotaSettings(input: LarkQuotaSettingsInput): Promise<LarkQuotaSettings> {
  return (await apiClient.put<LarkQuotaSettings>(path, input)).data
}
export async function testLarkQuotaNotification(): Promise<void> {
  await apiClient.post(`${path}/test`)
}
