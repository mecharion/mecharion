import { beforeEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { ref } from 'vue'
import Login from './Login.vue'

// 登录页(ADR-0037)。useChallenge 本身(PoW/WASM)在 useChallenge.test.ts —
// 不存在，PoW 求解的正确性在 pow.test.ts 已经钉住——这里只测 Login.vue
// 自己的逻辑：进度未就绪时挡提交、成功后跳转、失败后报错并换题。

const postMock = vi.fn()
vi.mock('../lib/api', () => ({ api: { post: (...args: unknown[]) => postMock(...args) } }))

const replaceMock = vi.fn()
vi.mock('vue-router', () => ({ useRouter: () => ({ replace: replaceMock }) }))

const refreshMock = vi.fn()
const answerMock = vi.fn()
const challenge = ref<{ background: string; piece: string; pieceY: number } | null>(null)
const progress = ref(0)
const chError = ref('')

vi.mock('../lib/useChallenge', () => ({
  useChallenge: () => ({
    challenge,
    progress,
    error: chError,
    sliderX: ref(0),
    refresh: refreshMock,
    answer: answerMock,
  }),
}))

beforeEach(() => {
  postMock.mockReset()
  replaceMock.mockReset()
  refreshMock.mockReset()
  answerMock.mockReset()
  challenge.value = { background: 'bg.png', piece: 'piece.png', pieceY: 10 }
  progress.value = 100
  chError.value = ''
})

function mountLogin() {
  return mount(Login, { global: { stubs: { SliderCaptcha: true } } })
}

async function typePasswordAndEnter(wrapper: ReturnType<typeof mountLogin>, password: string) {
  const pw = wrapper.find('input[type="password"]')
  await pw.setValue(password)
  await pw.trigger('keyup.enter')
}

describe('Login.vue', () => {
  it('进度到 100% 但 answer() 还没出来时，拒绝提交并提示', async () => {
    answerMock.mockReturnValue(null)
    const wrapper = mountLogin()
    await typePasswordAndEnter(wrapper, 'whatever')

    expect(wrapper.text()).toContain('验证还没算完，请稍候')
    expect(postMock).not.toHaveBeenCalled()
  })

  it('进度未满时，登录按钮被禁用', async () => {
    progress.value = 60
    const wrapper = mountLogin()
    const btn = wrapper.findAll('button').find((b) => b.text().includes('登录'))
    expect(btn?.attributes('disabled')).not.toBeUndefined()
  })

  it('登录成功后带着 challenge 答案提交，并跳转到首页', async () => {
    answerMock.mockReturnValue({ id: 'c1', pow: 42, sliderX: 7 })
    postMock.mockResolvedValueOnce(undefined)
    const wrapper = mountLogin()
    await typePasswordAndEnter(wrapper, 'my-password')
    await Promise.resolve()
    await Promise.resolve()

    expect(postMock).toHaveBeenCalledWith('/auth/login', {
      user: 'admin',
      password: 'my-password',
      challenge: { id: 'c1', pow: 42, sliderX: 7 },
    })
    expect(replaceMock).toHaveBeenCalledWith('/')
  })

  it('登录失败：报出服务端原因，且换一道新题（旧题已被服务端核销）', async () => {
    answerMock.mockReturnValue({ id: 'c1', pow: 42, sliderX: 7 })
    postMock.mockRejectedValueOnce(new Error('口令不对'))
    const wrapper = mountLogin()
    // 第一次 refresh 来自 onMounted
    refreshMock.mockClear()

    await typePasswordAndEnter(wrapper, 'wrong-password')
    await Promise.resolve()
    await Promise.resolve()
    await Promise.resolve()

    expect(wrapper.text()).toContain('口令不对')
    expect(replaceMock).not.toHaveBeenCalled()
    expect(refreshMock).toHaveBeenCalledTimes(1)
  })
})
