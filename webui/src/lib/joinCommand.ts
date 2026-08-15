/**
 * 把服务端给的加入命令里的主机占位符换成**浏览器此刻连着的地址**
 * （23-web-ui §4.6.1）。
 *
 * 服务端填不了这个：mechd 绑 `0.0.0.0`，它不知道自己对外是什么地址。
 * `Host` 头能拿到，但那是客户端可以随便写的东西——把它拼进一条要在别的
 * 机器上执行的命令里，等于让请求方决定新节点去连谁。
 *
 * 浏览器知道：它此刻正连着那个地址。
 */
const PLACEHOLDER = /https:\/\/<mechd-host>:\d+/

/**
 * @param command 服务端给的命令（含 `https://<mechd-host>:8443` 占位符）
 * @param origin  浏览器当前的 origin，例如 `https://10.0.0.1:8443`
 */
export function fillJoinHost(command: string, origin: string): string {
  if (!command) return ''
  // 没有占位符就原样返回：将来服务端若改成填好的地址，这里不该再动它
  if (!PLACEHOLDER.test(command)) return command
  return command.replace(PLACEHOLDER, origin.replace(/\/+$/, ''))
}

/**
 * 这个地址值不值得提醒一句。
 *
 * 用户可能通过一个**只有他能访问**的地址打开 UI（SSH 隧道、localhost、
 * 127.0.0.1），那时生成的命令会带上那个地址，而新节点连不上——
 * 症状是「按照界面给的命令敲了，加入失败」，而命令本身看起来完全正常。
 */
export function isLocalOrigin(origin: string): boolean {
  let host: string
  try {
    host = new URL(origin).hostname
  } catch {
    return false
  }
  return (
    host === 'localhost' ||
    host === '127.0.0.1' ||
    host === '::1' ||
    host === '[::1]' ||
    host.endsWith('.localhost')
  )
}
