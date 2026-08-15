import { beforeEach, describe, expect, it, vi } from 'vitest'
import { solvePow, WasmUnavailableError } from './pow'

// hash-wasm 的 argon2id 是真正做计算、依赖 WASM 的那一层——单测不该
// 真跑一遍 Argon2（慢，且这里要测的是「WASM 编译/实例化失败时该怎么
// 办」，不是「Argon2 算对了没有」，后者是 hash-wasm 自己的职责）。
// `vi.mock` 会被提升到文件顶部，早于上面的 import 执行，因此 pow.ts
// 里 `import { argon2id } from 'hash-wasm'` 拿到的就是这个桩。
const argon2idMock = vi.fn()
vi.mock('hash-wasm', () => ({ argon2id: (...args: unknown[]) => argon2idMock(...args) }))

// **每条测试重置**：mockResolvedValueOnce/mockRejectedValueOnce 排的队
// 不清空的话，前一条测试没消费完的返回值会漏到下一条，症状是调用次数
// 对不上、结果却像是「凑巧」对的——很难从失败信息看出真正原因。
beforeEach(() => {
  argon2idMock.mockReset()
})

const params = { salt: 'aabb', target: 'deadbeef', difficulty: 5, memory: 1024, time: 1 }

describe('solvePow', () => {
  it('第一次调用就失败——判定为环境跑不动 WASM，给出可读的错误', async () => {
    argon2idMock.mockRejectedValueOnce(
      new Error("WebAssembly.instantiate(): Wasm code generation disallowed by embedder"),
    )
    await expect(solvePow(params)).rejects.toBeInstanceOf(WasmUnavailableError)
  })

  it('前几次成功、后面才失败——原样抛出，不包装成"环境跑不动"', async () => {
    // n=0..2 都返回一个不匹配 target 的摘要（让循环继续往下试），
    // n=3 才抛错——此时已经证明 WASM 本身是好的，这次失败另有原因。
    argon2idMock
      .mockResolvedValueOnce('0000')
      .mockResolvedValueOnce('0001')
      .mockResolvedValueOnce('0002')
      .mockRejectedValueOnce(new Error('某种别的运行期错误'))

    await expect(solvePow(params)).rejects.toThrow('某种别的运行期错误')
  })

  it('算对了就返回那个 n，不多算一次', async () => {
    argon2idMock
      .mockResolvedValueOnce('0000')
      .mockResolvedValueOnce(params.target)
    const n = await solvePow(params)
    expect(n).toBe(1)
    expect(argon2idMock).toHaveBeenCalledTimes(2)
  })

  it('整个难度范围都没对上——报"没找到"，不是吞掉不响应', async () => {
    argon2idMock.mockResolvedValue('永远不匹配')
    await expect(solvePow(params)).rejects.toThrow('没能在难度范围内找到答案')
  })
})
