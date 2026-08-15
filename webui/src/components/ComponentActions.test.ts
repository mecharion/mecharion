import { beforeEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import ComponentActions from './ComponentActions.vue'
import type { RemovalImpact, RemoveResult, StatusView } from '../lib/api'

// 移除是整个界面唯一真正销毁东西的操作(10-cli §4.3/§7)。判据集中在
// 这条链路本身，不测 Element Plus 弹窗组件怎么画：
// ①二档确认——组件名打对了才放行，purgeData 时还要再确认一次；
// ②--purge-data 与「保留配置」这两个开关变化后必须重算 impact，
//   否则用户看到的是一个不会真正发生的删除范围；
// ③最终请求的 body 必须带上这次真正生效的 confirm/purgeData/keepConfig，
//   不能悄悄漏掉某个开关。

const delMock = vi.fn()
vi.mock('../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/api')>()
  return { ...actual, api: { del: (...args: unknown[]) => delMock(...args) } }
})

const promptMock = vi.fn()
const confirmMock = vi.fn()
const messageSuccess = vi.fn()
const messageError = vi.fn()
vi.mock('element-plus', async (importOriginal) => {
  const actual = await importOriginal<typeof import('element-plus')>()
  return {
    ...actual,
    // 属性值要是闭包，不能直接把 vi.fn() 的引用挂上去——`vi.mock` 工厂会被
    // 提升到文件顶部，早于下面 `const promptMock = vi.fn()` 执行；直接赋值
    // 会在提升后的求值时机读到一个还没初始化的变量。
    ElMessage: { success: (...args: unknown[]) => messageSuccess(...args), error: (...args: unknown[]) => messageError(...args) },
    ElMessageBox: { prompt: (...args: unknown[]) => promptMock(...args), confirm: (...args: unknown[]) => confirmMock(...args) },
  }
})

function status(over: Partial<StatusView> = {}): StatusView {
  return {
    component: 'web',
    pack: 'web',
    version: '1.0.0',
    instances: [],
    converged: true,
    ...over,
  }
}

function impact(over: Partial<RemovalImpact> = {}): RemovalImpact {
  return {
    component: 'web',
    pack: 'web',
    version: '1.0.0',
    nodes: ['n1', 'n2'],
    instances: 2,
    deleted: ['/var/lib/mecharion/web'],
    retained: [],
    ...over,
  }
}

beforeEach(() => {
  delMock.mockReset()
  promptMock.mockReset()
  confirmMock.mockReset()
  messageSuccess.mockReset()
  messageError.mockReset()
})

async function flush() {
  await Promise.resolve()
  await Promise.resolve()
  await Promise.resolve()
}

function mountActions() {
  return mount(ComponentActions, {
    props: { status: status() },
    global: { stubs: { 'router-link': true } },
  })
}

// 页面上有两个文字都叫"移除"的按钮：一个是打开对话框的触发按钮，
// 一个是对话框footer 里真正执行的确认按钮——不分清楚，测试点到的
// 永远是 findAll(...) 里排在前面那个（触发按钮），看起来像是在测
// doRemove，实际只是在反复重新打开对话框。
type ActionsWrapper = ReturnType<typeof mountActions>
function openTrigger(wrapper: ActionsWrapper) {
  return wrapper.findAll('button').find((b) => b.text() === '移除' && !b.element.closest('.el-dialog'))!
}
function confirmButton(wrapper: ActionsWrapper) {
  return wrapper.find('.el-dialog__footer').findAll('button').find((b) => b.text() === '移除')!
}

describe('ComponentActions.vue — 移除', () => {
  it('打开对话框先干跑一次，body 带 dryRun', async () => {
    delMock.mockResolvedValueOnce({ impact: impact(), dryRun: true } satisfies RemoveResult)
    const wrapper = mountActions()

    await openTrigger(wrapper).trigger('click')
    await flush()

    expect(delMock).toHaveBeenCalledWith('/components/web', {
      dryRun: true,
      purgeData: false,
      keepConfig: false,
    })
    expect(wrapper.text()).toContain('将移除 2 个实例')
  })

  it('确认输入框校验的是组件名，不是随便什么非空值', async () => {
    delMock.mockResolvedValueOnce({ impact: impact(), dryRun: true })
    const wrapper = mountActions()
    await openTrigger(wrapper).trigger('click')
    await flush()

    promptMock.mockRejectedValueOnce('cancel') // 用户取消，doRemove 提前返回
    await confirmButton(wrapper).trigger('click')
    await flush()

    const opts = promptMock.mock.calls[0][2]
    expect(opts.inputValidator('web')).toBe(true)
    expect(opts.inputValidator('wrong-name')).toBe('组件名不匹配')
  })

  it('打对组件名、不勾选 purgeData：直接放行，不弹第二道确认', async () => {
    delMock
      .mockResolvedValueOnce({ impact: impact(), dryRun: true } satisfies RemoveResult) // openRemove 的干跑
      .mockResolvedValueOnce({ impact: impact(), deleted: true } satisfies RemoveResult) // 真正执行

    const wrapper = mountActions()
    await openTrigger(wrapper).trigger('click')
    await flush()

    promptMock.mockResolvedValueOnce({ value: 'web' })
    await confirmButton(wrapper).trigger('click')
    await flush()

    expect(confirmMock).not.toHaveBeenCalled()
    expect(delMock).toHaveBeenLastCalledWith('/components/web', {
      confirm: 'web',
      purgeData: false,
      keepConfig: false,
    })
    expect(messageSuccess).toHaveBeenCalledWith('web 已移除')
  })

  it('勾选 purgeData 后再点确认框内的复选框，会重新拉一次 impact', async () => {
    delMock.mockResolvedValue({ impact: impact(), dryRun: true } satisfies RemoveResult)
    const wrapper = mountActions()
    await openTrigger(wrapper).trigger('click')
    await flush()
    delMock.mockClear()

    const purgeCheckbox = wrapper.findAll('.el-checkbox__original')[0]
    await purgeCheckbox.setValue(true)
    await flush()

    // 开关变了必须重算——不重算的话对话框上显示的删除范围是旧的、
    // 与即将真正执行的开关组合对不上
    expect(delMock).toHaveBeenCalledWith('/components/web', {
      dryRun: true,
      purgeData: true,
      keepConfig: false,
    })
  })

  it('purgeData=true 时：第二道确认必须先过，且请求体带着 purgeData:true', async () => {
    delMock
      .mockResolvedValueOnce({ impact: impact(), dryRun: true } satisfies RemoveResult)
      .mockResolvedValueOnce({ impact: impact(), dryRun: true } satisfies RemoveResult) // 勾选后重算
      .mockResolvedValueOnce({ impact: impact(), deleted: true } satisfies RemoveResult) // 真正执行

    const wrapper = mountActions()
    await openTrigger(wrapper).trigger('click')
    await flush()
    await wrapper.findAll('.el-checkbox__original')[0].setValue(true) // 勾 purgeData
    await flush()

    promptMock.mockResolvedValueOnce({ value: 'web' })
    confirmMock.mockResolvedValueOnce('confirm')
    await confirmButton(wrapper).trigger('click')
    await flush()

    expect(confirmMock).toHaveBeenCalledOnce()
    expect(delMock).toHaveBeenLastCalledWith('/components/web', {
      confirm: 'web',
      purgeData: true,
      keepConfig: false,
    })
  })

  it('purgeData=true 时，第二道确认被取消：数据不会被删', async () => {
    delMock
      .mockResolvedValueOnce({ impact: impact(), dryRun: true } satisfies RemoveResult)
      .mockResolvedValueOnce({ impact: impact(), dryRun: true } satisfies RemoveResult)

    const wrapper = mountActions()
    await openTrigger(wrapper).trigger('click')
    await flush()
    await wrapper.findAll('.el-checkbox__original')[0].setValue(true)
    await flush()

    promptMock.mockResolvedValueOnce({ value: 'web' })
    confirmMock.mockRejectedValueOnce('cancel')
    const callsBefore = delMock.mock.calls.length
    await confirmButton(wrapper).trigger('click')
    await flush()

    // 第一道过了、第二道没过：不应该再多一次真正执行的 DELETE
    expect(delMock.mock.calls.length).toBe(callsBefore)
    expect(messageSuccess).not.toHaveBeenCalled()
  })
})
