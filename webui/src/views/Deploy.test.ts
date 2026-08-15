import { beforeEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import Deploy from './Deploy.vue'
import type { NodeView, PackView, PacksView } from '../lib/api'

// 部署页(23-web-ui §2.7)。判据集中在三处真会「错了就毁东西/骗用户」的
// 逻辑：①只有在线且未暂停/未吊销的节点可选,原因文案要对得上状态；
// ②预览与部署必须传同一份 body,只有 dryRun 不同,否则「预览说没事」
// 和「部署做的事」可能对不上；③上传成功要清楚地说「这只是收进集合，
// 不是部署」——已经在模板文案里，这里测的是它不会被静默吞掉/搞混
// 上传态与部署态。

const getMock = vi.fn()
const postMock = vi.fn()
const uploadMock = vi.fn()
vi.mock('../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../lib/api')>()
  return {
    ...actual,
    api: {
      get: (...args: unknown[]) => getMock(...args),
      post: (...args: unknown[]) => postMock(...args),
      upload: (...args: unknown[]) => uploadMock(...args),
    },
  }
})

const pushMock = vi.fn()
vi.mock('vue-router', () => ({ useRouter: () => ({ push: pushMock }) }))

function pack(over: Partial<PackView> = {}): PackView {
  return { name: 'web', versions: ['1.0.0'], ...over }
}
function node(over: Partial<NodeView> = {}): NodeView {
  return { name: 'n1', address: '10.0.0.1', status: 'online', ...over }
}

beforeEach(() => {
  getMock.mockReset()
  postMock.mockReset()
  uploadMock.mockReset()
  pushMock.mockReset()
})

function mountDeploy() {
  return mount(Deploy, { global: { stubs: { AppShell: { template: '<div><slot /></div>' } } } })
}

async function flush() {
  await Promise.resolve()
  await Promise.resolve()
  await Promise.resolve()
}

describe('Deploy.vue', () => {
  it('离线/暂停/吊销的节点不可选，且原因文案分得清', async () => {
    const packs: PacksView = { packs: [pack()] }
    getMock.mockImplementation((p: string) => {
      if (p === '/packs') return Promise.resolve(packs)
      if (p === '/nodes')
        return Promise.resolve({
          nodes: [
            node({ name: 'ok', status: 'online' }),
            node({ name: 'pending', status: 'pending' }),
            node({ name: 'off', status: 'offline' }),
            node({ name: 'cordoned', status: 'online', cordoned: true }),
            node({ name: 'revoked', status: 'online', revoked: true }),
          ],
        })
      return Promise.reject(new Error(`unexpected GET ${p}`))
    })

    const wrapper = mountDeploy()
    await flush()
    // 节点表格只在选中 Pack 之后才渲染（`v-if="pack"`）
    await wrapper.find('.el-table__row').trigger('click')
    await flush()

    const text = wrapper.text()
    expect(text).toContain('还没加入')
    expect(text).toContain('已掉线')
    expect(text).toContain('已暂停调和')
    expect(text).toContain('证书已吊销')
  })

  it('选中 Pack 后组件名默认等于 Pack 名；预览与部署的 body 只有 dryRun 不同', async () => {
    const packs: PacksView = { packs: [pack({ name: 'db' })] }
    getMock.mockImplementation((p: string) => {
      if (p === '/packs') return Promise.resolve(packs)
      if (p === '/nodes') return Promise.resolve({ nodes: [node({ name: 'n1' })] })
      return Promise.reject(new Error(`unexpected GET ${p}`))
    })
    postMock.mockImplementation((p: string, body: unknown) => {
      if (p === '/components') return Promise.resolve({ instances: ['n1/db/0'], warnings: [] })
      return Promise.reject(new Error(`unexpected POST ${p} ${JSON.stringify(body)}`))
    })

    const wrapper = mountDeploy()
    await flush()

    // 选 Pack：点表格行触发 current-change
    await wrapper.find('.el-table__row').trigger('click')
    await flush()
    expect((wrapper.find('input.el-input__inner').element as HTMLInputElement).value).toBe('db')

    // 选节点：勾选框（限定在 tbody 内，避开表头的"全选"checkbox）
    const checkbox = wrapper.find('tbody .el-checkbox__original')
    await checkbox.setValue(true)
    await flush()

    const preview = wrapper.findAll('button').find((b) => b.text() === '预览')!
    await preview.trigger('click')
    await flush()

    const deploy = wrapper.findAll('button').find((b) => b.text() === '部署')!
    expect(deploy.attributes('disabled')).toBeUndefined()
    await deploy.trigger('click')
    await flush()

    expect(postMock).toHaveBeenCalledTimes(2)
    const [previewCall, deployCall] = postMock.mock.calls
    expect(previewCall[1]).toMatchObject({ pack: 'db', component: 'db', dryRun: true })
    expect(deployCall[1]).toMatchObject({ pack: 'db', component: 'db', dryRun: false })
    // 除了 dryRun，其余字段必须相同——预览说的和实际部署的是同一件事
    expect({ ...previewCall[1], dryRun: undefined }).toEqual({ ...deployCall[1], dryRun: undefined })

    expect(pushMock).toHaveBeenCalledWith('/components/db')
  })

  it('上传成功：明确提示"只是收进集合"，并触发 packs 重新拉取', async () => {
    getMock.mockImplementation((p: string) => {
      if (p === '/packs') return Promise.resolve({ packs: [] } satisfies PacksView)
      if (p === '/nodes') return Promise.resolve({ nodes: [] })
      return Promise.reject(new Error(`unexpected GET ${p}`))
    })
    uploadMock.mockResolvedValueOnce({
      name: 'hello',
      version: '0.1.0',
      revision: 1,
      digest: 'a'.repeat(64),
      size: 2048,
    })

    const wrapper = mountDeploy()
    await flush()
    getMock.mockClear()

    const file = new File([new Uint8Array([1, 2, 3])], 'hello.mpack')
    const input = wrapper.find('input[type="file"]')
    Object.defineProperty(input.element, 'files', { value: [file] })
    await input.trigger('change')
    await flush()

    expect(uploadMock).toHaveBeenCalledWith('/packs', file)
    expect(wrapper.text()).toContain('已收下 hello 0.1.0')
    // 重新拉取 packs 列表，而不是本地拼一条假数据
    expect(getMock).toHaveBeenCalledWith('/packs')
  })

  it('上传被拒：显示原因，且不假装成功', async () => {
    getMock.mockImplementation((p: string) => {
      if (p === '/packs') return Promise.resolve({ packs: [] } satisfies PacksView)
      if (p === '/nodes') return Promise.resolve({ nodes: [] })
      return Promise.reject(new Error(`unexpected GET ${p}`))
    })
    const { ApiError } = await import('../lib/api')
    uploadMock.mockRejectedValueOnce(new ApiError(400, '摘要不匹配'))

    const wrapper = mountDeploy()
    await flush()

    const file = new File([new Uint8Array([1])], 'bad.mpack')
    const input = wrapper.find('input[type="file"]')
    Object.defineProperty(input.element, 'files', { value: [file] })
    await input.trigger('change')
    await flush()

    expect(wrapper.text()).toContain('摘要不匹配')
    expect(wrapper.text()).not.toContain('已收下')
  })
})
