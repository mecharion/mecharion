import { argon2id } from 'hash-wasm'

// 解 PoW：找出那个让 argon2id(salt, n) 等于服务端目标的 n。
//
// **不对称是成本的来源**：服务端出题与核对各只算一次，而这里平均要算
// difficulty/2 次。
//
// 用 Argon2（内存硬）而不是 SHA-256：后者对 GPU 友好，攻击者用一张显卡就能
// 把「成本」这件事抹平（ADR-0037）。代价是浏览器要靠 WASM 才算得动，
// 这也是引入 hash-wasm 的唯一理由。

export interface PowParams {
  salt: string // hex
  target: string // hex
  difficulty: number
  memory: number // KiB
  time: number
}

/**
 * WASM 编译/实例化失败——环境本身跑不动 PoW，不是算错了。
 *
 * **没有兜底方案**：换一套更弱/更快的纯 JS 实现会抵消 Argon2 的内存硬
 * 成本（ADR-0037 的立论基础），换一套同样慢的纯 JS 实现又没有理由不
 * 直接修好 WASM。因此这里只把"为什么失败"从一句浏览器内部报错
 * （因引擎而异，多半读不懂）变成一句人能看懂、能照做的话，见
 * `useChallenge.ts` 的错误文案。
 */
export class WasmUnavailableError extends Error {
  constructor(cause: unknown) {
    super('这个环境无法运行 WebAssembly，PoW 验证需要它才能完成')
    this.name = 'WasmUnavailableError'
    this.cause = cause
  }
}

function hexToBytes(hex: string): Uint8Array {
  const out = new Uint8Array(hex.length / 2)
  for (let i = 0; i < out.length; i++) {
    out[i] = parseInt(hex.slice(i * 2, i * 2 + 2), 16)
  }
  return out
}

/**
 * 逐个试 n，直到摘要与目标一致。
 *
 * onProgress 让界面能显示进度——这一步要花约一秒，没有反馈的话用户会以为
 * 页面卡住了。
 */
export async function solvePow(
  p: PowParams,
  onProgress?: (done: number, total: number) => void,
): Promise<number> {
  const salt = hexToBytes(p.salt)
  for (let n = 0; n < p.difficulty; n++) {
    let digest: string
    try {
      digest = await argon2id({
        password: String(n),
        salt,
        parallelism: 1,
        iterations: p.time,
        memorySize: p.memory,
        hashLength: 16,
        outputType: 'hex',
      })
    } catch (e) {
      // **只在第一次调用时判定为"环境跑不动"**：WASM 编译/实例化是
      // 一次性的、要么全程能跑要么从第一次就不能跑——不是偶发失败，
      // 没有理由重试。n>0 时还失败，说明前面几次已经成功过，这次的
      // 错误另有原因，原样抛出更诚实。
      if (n === 0) throw new WasmUnavailableError(e)
      throw e
    }
    if (digest === p.target) return n
    if (onProgress && n % 5 === 0) onProgress(n, p.difficulty)
  }
  throw new Error('没能在难度范围内找到答案')
}
