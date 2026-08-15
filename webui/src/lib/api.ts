// 与 mechd 说话的薄封装。
//
// **写操作一律带 CSRF 头**：浏览器不允许跨源请求带自定义头，因此带得上它
// 的请求一定来自我们自己的页面。它与 cookie 的 SameSite=Strict 是两道，
// 任何一道单独失效另一道还在。
const CSRF_HEADER = 'X-Mecharion-Request'

/**
 * 转义一个会被拼进 API path 的用户可控标识符（组件名/节点名/配置组名/
 * 角色名等）。
 *
 * **纵深防御，不是唯一防线**：这些标识符在 mechd 侧已经由
 * `internal/ident` 在写入时校验过字符集，落库之后的名字不可能带
 * '/'、空格或其它特殊字符。但这里拼路径的时机往往**早于**那次校验
 * ——比如"新建配置组"时，用户刚敲的名字还没提交给服务端判断合不
 * 合法就先要拼进 PUT 的 URL。不转义的话，一个带 '/' 的名字会把请求
 * 错路由到别的接口（甚至命中一个语义完全不同的路径），而不是拿到
 * 服务端本该给的、干净的 400——转义之后最坏情况也只是原样把这个
 * （最终会被判定非法的）字符串送到服务端。
 */
export function seg(s: string | number): string {
  return encodeURIComponent(String(s))
}

/**
 * 把非空的查询参数编码成 "?a=b&c=d"；全部为空时返回空串。
 *
 * 用 URLSearchParams 而不是手写 `"?" + "k=" + v + "&..."` 拼接——后者
 * 对值里本身含 '&'/'='/空格的情况（角色名同样是用户可控字符串）没有
 * 任何转义。
 */
export function apiQuery(kv: Record<string, string | undefined>): string {
  const q = new URLSearchParams()
  for (const [k, v] of Object.entries(kv)) {
    if (v) q.set(k, v)
  }
  const s = q.toString()
  return s ? `?${s}` : ''
}

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message)
  }
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = {}
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  if (method !== 'GET' && method !== 'HEAD') headers[CSRF_HEADER] = '1'

  const res = await fetch(`/api/v1${path}`, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
    // cookie 是 HttpOnly 的，脚本读不到它；这里只是让浏览器带上
    credentials: 'same-origin',
  })

  const text = await res.text()
  // any 是故意的：这是解析服务端 JSON 的边界，形状由 T 在调用方声明，
  // 这里不该重复定义一遍——unknown 只会逼着下面的 data?.error 多写一次
  // 类型断言，换不到真实的安全性。
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  let data: any = null
  if (text) {
    try {
      data = JSON.parse(text)
    } catch {
      data = { error: text }
    }
  }
  if (!res.ok) {
    throw new ApiError(res.status, data?.error ?? `HTTP ${res.status}`)
  }
  return data as T
}

/**
 * 上传一个二进制文件。
 *
 * **不走 multipart**：服务端直接读请求体，因此这里也直接发 Blob。
 * multipart 要求服务端先把整个请求缓冲下来才能拿到那个 part，而 Pack
 * 动辄几百 MB。
 */
async function upload<T>(path: string, file: File): Promise<T> {
  const res = await fetch(`/api/v1${path}`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/octet-stream',
      [CSRF_HEADER]: '1',
    },
    body: file,
    credentials: 'same-origin',
  })
  const text = await res.text()
  // any 是故意的：这是解析服务端 JSON 的边界，形状由 T 在调用方声明，
  // 这里不该重复定义一遍——unknown 只会逼着下面的 data?.error 多写一次
  // 类型断言，换不到真实的安全性。
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  let data: any = null
  if (text) {
    try {
      data = JSON.parse(text)
    } catch {
      data = { error: text }
    }
  }
  if (!res.ok) throw new ApiError(res.status, data?.error ?? `HTTP ${res.status}`)
  return data as T
}

export const api = {
  upload,
  get: <T>(p: string) => request<T>('GET', p),
  post: <T>(p: string, body?: unknown) => request<T>('POST', p, body ?? {}),
  patch: <T>(p: string, body?: unknown) => request<T>('PATCH', p, body ?? {}),
  put: <T>(p: string, body?: unknown) => request<T>('PUT', p, body ?? {}),
  // DELETE **要能带请求体**：移除与清理的确认串走请求体而不是 query，
  // 因为 URL 会进网关日志与浏览器历史，而那是唯一一个「打错就毁数据」
  // 的参数。
  del: <T>(p: string, body?: unknown) => request<T>('DELETE', p, body),
}

export interface AuthState {
  initialized: boolean
  user: string
}

export interface Challenge {
  id: string
  powSalt: string
  powTarget: string
  powDifficulty: number
  powMemory: number
  powTime: number
  background: string
  piece: string
  pieceY: number
}

export interface OrphanView {
  instance: string
  firstSeen: string
}

export interface NodeView {
  name: string
  address: string
  status: string
  labels?: Record<string, string>
  // cordoned 与 revoked 是**两件不同的事**，因此是两个字段而不是塞进 status：
  // 一台机器可以既在线又被暂停调和，并成一列会丢信息。
  cordoned?: boolean
  revoked?: boolean
  orphans?: OrphanView[]
}

export interface ComponentView {
  name: string
  pack: string
  version: string
  profile?: string
  instances: number
  // removing = 正在被移除，不接受任何写操作
  removing?: boolean
}

/** 一次移除会发生什么。字段与 mechd.RemovalImpact 一一对应。 */
export interface RemovalImpact {
  component: string
  pack: string
  version: string
  nodes?: string[]
  instances: number
  // deleted / retained 两边都要显示：只给一边，用户无从判断这次删除有多狠
  deleted?: string[]
  retained?: string[]
  removing?: boolean
  progress?: { total: number; done: number; pending?: string[] }
}

export interface RemoveResult {
  impact: RemovalImpact
  notFound?: boolean
  deleted?: boolean
  dryRun?: boolean
}

/** 一个孤儿：机器上还在、控制面不认的东西。 */
export interface OrphanEntry {
  node: string
  instance: string
  // paths 为空 = 它不是数据残留，而是**整个实例还装着、可能还在跑**
  paths?: string[]
  firstSeen: string
  lastSeen: string
  purgeRequested?: boolean
}

export interface DriftView {
  resource: string
  policy?: string
  detail?: string
}

export interface InstanceView {
  role: string
  node: string
  ordinal: number
  want: string
  got?: string
  converged: boolean
  result?: string
  workload?: string
  restarts?: number
  health?: string
  generation?: number
  reportedAt?: string
  runState?: string
  workloadAction?: string
  workloadActionAt?: string
  rolledBack?: boolean
  // pendingVersion 非空 = 这台**还没轮到**，在排队而不是出了问题
  pendingVersion?: string
  drift?: DriftView[]
  suppressed?: string[]
}

export interface StatusView {
  component: string
  pack: string
  version: string
  profile?: string
  instances: InstanceView[]
  converged: boolean
  warnings?: string[]
}

export interface RolloutView {
  id: number
  state: string
  kind: string
  from?: string
  to: string
  reason?: string
  startedAt: string
  endedAt?: string
  batch?: number
  batches?: number
  current?: string
  skipped?: string[]
  mixed?: Record<string, string[]>
}

/**
 * 表单里的一个字段。
 *
 * 字段名与 render.FormParam 一一对应。**不要在前端补默认值**：
 * 「服务端没给」与「服务端给了 false」在这里是两件事，而补默认值会
 * 把前者变成后者——一个 immutable 参数因此变得可编辑。
 */
export interface FormParam {
  name: string
  type: string
  description?: string
  unit?: string
  group?: string
  advanced?: boolean
  required?: boolean
  min?: unknown
  max?: unknown
  pattern?: string
  values?: unknown[]
  default?: unknown
  value?: unknown
  /** default | component | role | group | derived | generated | defaultFrom */
  source: string
  /** 仅 secret：设过没有。**永远没有值本身，也没有长度** */
  set?: boolean
  readOnly?: boolean
  immutable?: boolean
  sensitive?: boolean
  restartRequired?: boolean
  reloadRequired?: boolean
  /** 现在算不出来（未部署组件上的 from）。与「空值」不是一回事 */
  pending?: boolean
}

export interface GroupView {
  name: string
  members: string[]
}

export interface FormView {
  component: string
  pack: string
  version: string
  profile?: string
  roles: string[]
  role: string
  groups?: GroupView[]
  group?: string
  params: FormParam[]
  warnings?: string[]
}

/** 一次参数变更影响到的实例。 */
export interface ParamChange {
  role: string
  node: string
  from: string
  to: string
}

/**
 * PATCH /components/{name}/params 的结果。
 *
 * `effect` 是**这一次改动的合并结论**（restart | reload | none），
 * 不是逐参数的标志——用户问的是「我点下去会发生什么」。
 */
export interface SetParamsResult {
  component: string
  changed?: ParamChange[]
  effect: string
  restarted?: string[]
  reloaded?: string[]
  warnings?: string[]
  dryRun: boolean
}

export interface PackView {
  name: string
  description?: string
  keywords?: string[]
  versions: string[]
  roles?: string[]
  profiles?: string[]
  deployed?: string[]
}

export interface PackProblem {
  dir: string
  reason: string
}

export interface PacksView {
  packs: PackView[]
  problems?: PackProblem[]
}

/** 一个配置组。 */
export interface ConfigGroupDetail {
  name: string
  role: string
  members: string[]
  params?: Record<string, unknown>
  paths?: Record<string, string[]>
}

/**
 * SSE 推送的一份完整状态（23-web-ui §4.5.1）。
 *
 * **不是增量**：处理方式是「用它覆盖手上那份」。
 */
export interface WatchSnapshot {
  component: string
  status?: StatusView
  rollout?: RolloutView
  at: string
}

/** 一张 join token。**明文只在创建时回一次**（23-web-ui §4.6.2）。 */
export interface JoinTokenView {
  id: number
  /** 仅创建响应里有；列表里永远没有 */
  token?: string
  node?: string
  expires: string
  maxUses: number
  used: number
  revoked?: boolean
  usable: boolean
  reason?: string
  /** 可粘贴的加入命令，含 CA 公钥指纹；主机名由前端填 */
  joinCommand?: string
}

/** 一次 Pack 上传的结果。 */
export interface UploadResult {
  name: string
  version: string
  revision: number
  digest: string
  size: number
  warnings?: string[]
  replaced?: boolean
}
