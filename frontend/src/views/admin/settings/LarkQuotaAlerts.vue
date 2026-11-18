<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { getLarkQuotaSettings, updateLarkQuotaSettings, testLarkQuotaNotification, type LarkQuotaSettings } from '@/api/admin/larkQuotaAlerts'

const { t } = useI18n()
const key = 'admin.settings.larkQuota.'
const state = ref<LarkQuotaSettings>({ enabled: false, thresholds: [95, 100], windows: ['5h', '7d'], webhook_configured: false, signing_secret_configured: false })
const thresholds = ref('95, 100')
const webhook = ref('')
const secret = ref('')
const clearWebhook = ref(false)
const clearSecret = ref(false)
const loading = ref(true)
const loaded = ref(false)
const saving = ref(false)
const testing = ref(false)
const error = ref('')
const status = ref('')
const saved = ref('')
const snapshot = computed(() => JSON.stringify([state.value.enabled, state.value.windows, thresholds.value]))
const dirty = computed(() => snapshot.value !== saved.value || !!webhook.value || !!secret.value || clearWebhook.value || clearSecret.value)
const busy = computed(() => loading.value || saving.value || testing.value)

function apply(value: LarkQuotaSettings) {
  state.value = value
  thresholds.value = value.thresholds.join(', ')
  webhook.value = secret.value = ''
  clearWebhook.value = clearSecret.value = false
  saved.value = snapshot.value
}

async function load() {
  loading.value = true
  error.value = ''
  try { apply(await getLarkQuotaSettings()); loaded.value = true }
  catch { error.value = t(key + 'loadFailed') }
  finally { loading.value = false }
}

async function save() {
  const parts = thresholds.value.split(/[,，]/).map(v => v.trim())
  const values = parts.map(Number)
  if (!parts.length || parts.length > 10 || parts.some(v => !v) || values.some(v => !Number.isFinite(v) || v <= 0 || v > 100) || !state.value.windows.length) {
    error.value = t(key + 'invalid'); return
  }
  saving.value = true
  error.value = status.value = ''
  try {
    apply(await updateLarkQuotaSettings({ enabled: state.value.enabled, thresholds: [...new Set(values)].sort((a, b) => a - b), windows: state.value.windows,
      webhook: clearWebhook.value ? '' : webhook.value, signing_secret: clearSecret.value ? '' : secret.value, clear_webhook: clearWebhook.value, clear_signing_secret: clearSecret.value }))
    status.value = t(key + 'saved')
  } catch { error.value = t(key + 'saveFailed') }
  finally { saving.value = false }
}

async function test() {
  testing.value = true
  error.value = status.value = ''
  try { await testLarkQuotaNotification(); status.value = t(key + 'testSent') }
  catch { error.value = t(key + 'testFailed') }
  finally { testing.value = false }
}

onMounted(load)
</script>

<template>
  <section class="card" :aria-label="t(key + 'title')">
    <div class="border-b border-gray-100 px-6 py-4 dark:border-dark-700">
      <h3 class="text-base font-medium text-gray-900 dark:text-white">{{ t(key + 'title') }}</h3>
      <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">{{ t(key + 'description') }}</p>
    </div>
    <div class="space-y-4 px-6 py-6">
      <p v-if="error" role="alert" class="text-sm text-red-600">{{ error }}</p>
      <p v-if="status" role="status" class="text-sm text-green-600">{{ status }}</p>
      <button v-if="!loading && !loaded" type="button" class="btn btn-secondary" @click="load">{{ t(key + 'reload') }}</button>
      <fieldset :disabled="busy || !loaded" class="space-y-4">
        <label class="flex items-center gap-2"><input v-model="state.enabled" data-testid="enabled" type="checkbox">{{ t(key + 'enabled') }}</label>
        <div>
          <label for="lark-quota-webhook" class="input-label">{{ t(key + 'webhook') }} · {{ t(key + (state.webhook_configured ? 'configured' : 'notConfigured')) }}</label>
          <input id="lark-quota-webhook" v-model="webhook" type="password" autocomplete="new-password" class="input" :disabled="clearWebhook" :placeholder="t(key + 'secretHint')">
          <label v-if="state.webhook_configured" class="mt-2 flex items-center gap-2 text-sm"><input v-model="clearWebhook" data-testid="clear-webhook" type="checkbox">{{ t(key + 'clearWebhook') }}</label>
        </div>
        <div>
          <label for="lark-quota-secret" class="input-label">{{ t(key + 'secret') }} · {{ t(key + (state.signing_secret_configured ? 'configured' : 'notConfigured')) }}</label>
          <input id="lark-quota-secret" v-model="secret" type="password" autocomplete="new-password" class="input" :disabled="clearSecret" :placeholder="t(key + 'secretHint')">
          <label v-if="state.signing_secret_configured" class="mt-2 flex items-center gap-2 text-sm"><input v-model="clearSecret" data-testid="clear-secret" type="checkbox">{{ t(key + 'clearSecret') }}</label>
        </div>
        <div>
          <label for="lark-quota-thresholds" class="input-label">{{ t(key + 'thresholds') }}</label>
          <input id="lark-quota-thresholds" v-model="thresholds" class="input" inputmode="decimal" placeholder="95, 100">
          <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t(key + 'thresholdHint') }}</p>
        </div>
        <div class="flex flex-wrap gap-4">
          <span>{{ t(key + 'windows') }}</span>
          <label class="flex items-center gap-2"><input v-model="state.windows" type="checkbox" value="5h">{{ t(key + 'fiveHour') }}</label>
          <label class="flex items-center gap-2"><input v-model="state.windows" type="checkbox" value="7d">{{ t(key + 'sevenDay') }}</label>
        </div>
        <p class="text-xs text-gray-500 dark:text-gray-400">{{ t(key + 'dataHint') }}</p>
        <div class="flex flex-wrap gap-3">
          <button type="button" class="btn btn-primary" data-testid="save" @click="save">{{ t(key + (saving ? 'saving' : 'save')) }}</button>
          <button type="button" class="btn btn-secondary" data-testid="test" :disabled="dirty || !state.webhook_configured" @click="test">{{ t(key + (testing ? 'testing' : 'test')) }}</button>
        </div>
      </fieldset>
    </div>
  </section>
</template>
