import { beforeEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import Setup from './Setup.vue'

// 初始化页(ADR-0037/0039)。判据集中在门禁本身：口令长度、两次输入一致、
// 令牌必填——这三条错一条，轻则用户白填一次表单，重则一个弱口令或
// 空令牌被送到服务端(服务端会再判一次，但前端错误的判据会让人以为
// 「通过前端检查=一定成功」，与实际不符时体验很差)。

const postMock = vi.fn()
vi.mock('../lib/api', () => ({ api: { post: (...args: unknown[]) => postMock(...args) } }))

const replaceMock = vi.fn()
vi.mock('vue-router', () => ({ useRouter: () => ({ replace: replaceMock }) }))

beforeEach(() => {
  postMock.mockReset()
  replaceMock.mockReset()
})

async function fillAndSubmit(wrapper: ReturnType<typeof mount>, password: string, confirm: string, token: string) {
  const inputs = wrapper.findAll('input')
  // 顺序对应模板：账号(disabled)、设定口令、再输一次、初始化令牌
  await inputs[1].setValue(password)
  await inputs[2].setValue(confirm)
  await inputs[3].setValue(token)
  await wrapper.find('form').trigger('submit')
}

describe('Setup.vue', () => {
  it('口令太短时拒绝提交，不调用 API', async () => {
    const wrapper = mount(Setup)
    await fillAndSubmit(wrapper, 'short', 'short', 'tok')
    expect(wrapper.text()).toContain('口令至少 12 个字符')
    expect(postMock).not.toHaveBeenCalled()
  })

  it('两次口令不一致时拒绝提交', async () => {
    const wrapper = mount(Setup)
    await fillAndSubmit(wrapper, 'a-long-enough-password', 'different-password-here', 'tok')
    expect(wrapper.text()).toContain('两次输入不一致')
    expect(postMock).not.toHaveBeenCalled()
  })

  it('没填令牌时拒绝提交', async () => {
    const wrapper = mount(Setup)
    await fillAndSubmit(wrapper, 'a-long-enough-password', 'a-long-enough-password', '')
    expect(wrapper.text()).toContain('请填写初始化令牌')
    expect(postMock).not.toHaveBeenCalled()
  })

  it('校验都过了就提交，成功后跳去登录页', async () => {
    postMock.mockResolvedValueOnce(undefined)
    const wrapper = mount(Setup)
    await fillAndSubmit(wrapper, 'a-long-enough-password', 'a-long-enough-password', 'the-admin-token')

    expect(postMock).toHaveBeenCalledWith('/auth/bootstrap', {
      password: 'a-long-enough-password',
      token: 'the-admin-token',
    })
    // 等 submit() 里的 await 落地
    await Promise.resolve()
    await Promise.resolve()
    expect(replaceMock).toHaveBeenCalledWith('/login')
  })

  it('服务端拒绝时显示原因，不跳转', async () => {
    postMock.mockRejectedValueOnce(new Error('令牌不对'))
    const wrapper = mount(Setup)
    await fillAndSubmit(wrapper, 'a-long-enough-password', 'a-long-enough-password', 'wrong-token')

    await Promise.resolve()
    await Promise.resolve()
    await Promise.resolve()
    expect(wrapper.text()).toContain('令牌不对')
    expect(replaceMock).not.toHaveBeenCalled()
  })
})
