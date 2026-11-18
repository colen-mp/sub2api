import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import LarkQuotaAlerts from '../LarkQuotaAlerts.vue'
import { getLarkQuotaSettings, updateLarkQuotaSettings, testLarkQuotaNotification } from '@/api/admin/larkQuotaAlerts'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
vi.mock('@/api/admin/larkQuotaAlerts', () => ({ getLarkQuotaSettings: vi.fn(), updateLarkQuotaSettings: vi.fn(), testLarkQuotaNotification: vi.fn() }))

const saved = { enabled: true, thresholds: [95, 100], windows: ['5h', '7d'] as ('5h' | '7d')[], webhook_configured: true, signing_secret_configured: true }

describe('LarkQuotaAlerts', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    vi.mocked(getLarkQuotaSettings).mockResolvedValue({ ...saved, windows: [...saved.windows] })
    vi.mocked(updateLarkQuotaSettings).mockResolvedValue({ ...saved })
    vi.mocked(testLarkQuotaNotification).mockResolvedValue()
  })

  it('loads configured flags without exposing secrets or sending a message', async () => {
    const wrapper = mount(LarkQuotaAlerts)
    await flushPromises()
    expect((wrapper.get('#lark-quota-webhook').element as HTMLInputElement).value).toBe('')
    expect((wrapper.get('#lark-quota-secret').element as HTMLInputElement).value).toBe('')
    expect(wrapper.get('[data-testid="test"]').attributes('disabled')).toBeUndefined()
    expect(testLarkQuotaNotification).not.toHaveBeenCalled()
  })

  it('saves edited thresholds while preserving blank secrets and never auto-tests', async () => {
    const wrapper = mount(LarkQuotaAlerts); await flushPromises()
    await wrapper.get('#lark-quota-thresholds').setValue('100, 90, 90')
    expect(wrapper.get('[data-testid="test"]').attributes('disabled')).toBeDefined()
    await wrapper.get('[data-testid="save"]').trigger('click'); await flushPromises()
    expect(updateLarkQuotaSettings).toHaveBeenCalledWith(expect.objectContaining({ thresholds: [90, 100], webhook: '', signing_secret: '', clear_webhook: false }))
    expect(testLarkQuotaNotification).not.toHaveBeenCalled()
  })

  it('clears credentials explicitly and discards an unsaved replacement', async () => {
    const wrapper = mount(LarkQuotaAlerts); await flushPromises()
    await wrapper.get('#lark-quota-webhook').setValue('synthetic-replacement')
    await wrapper.get('[data-testid="clear-webhook"]').setValue(true)
    await wrapper.get('[data-testid="clear-secret"]').setValue(true)
    await wrapper.get('[data-testid="save"]').trigger('click'); await flushPromises()
    expect(updateLarkQuotaSettings).toHaveBeenCalledWith(expect.objectContaining({ clear_webhook: true, clear_signing_secret: true, webhook: '', signing_secret: '' }))
  })

  it.each(['', '0, 100', '95, 101', '95,,100', 'NaN'])('rejects invalid thresholds %s', async value => {
    const wrapper = mount(LarkQuotaAlerts); await flushPromises()
    await wrapper.get('#lark-quota-thresholds').setValue(value)
    await wrapper.get('[data-testid="save"]').trigger('click')
    expect(wrapper.find('[role="alert"]').exists()).toBe(true)
    expect(updateLarkQuotaSettings).not.toHaveBeenCalled()
  })

  it('tests only the saved robot on an explicit click', async () => {
    const wrapper = mount(LarkQuotaAlerts); await flushPromises()
    await wrapper.get('[data-testid="test"]').trigger('click'); await flushPromises()
    expect(testLarkQuotaNotification).toHaveBeenCalledOnce()
    expect(wrapper.get('[role="status"]').text()).toContain('testSent')
  })

  it('disables testing without a saved robot', async () => {
    vi.mocked(getLarkQuotaSettings).mockResolvedValue({ ...saved, enabled: false, webhook_configured: false })
    const wrapper = mount(LarkQuotaAlerts); await flushPromises()
    expect(wrapper.get('[data-testid="test"]').attributes('disabled')).toBeDefined()
  })

  it('shows failures without rendering raw errors that might contain tokens', async () => {
    vi.mocked(testLarkQuotaNotification).mockRejectedValue(new Error('private-token'))
    const wrapper = mount(LarkQuotaAlerts); await flushPromises()
    await wrapper.get('[data-testid="test"]').trigger('click'); await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toContain('testFailed')
    expect(wrapper.html()).not.toContain('private-token')
  })
})
