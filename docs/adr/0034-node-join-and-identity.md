# ADR-0034: 节点加入以 token 为授权，身份以证书 CN 为准

- **状态**：已接受
- **日期**：2026-08-06
- **相关**：[ADR-0001](0001-agent-based.md)、[ADR-0014](0014-no-ha-in-v1.md)、[ADR-0026](0026-standalone-runs-mechd.md)

## 背景

到 M6 为止，Mecharion **只能管一台机器**——不是不好用，是真的做不到：
全仓库只有 `mechlet install --standalone` 会往 `nodes` 表写行，而它写的是
本机；`Register` 拒绝不在册的节点，却让用户去敲一条并不存在的
`mechctl node add`；mechd 也只监听 unix socket。

M7 要让第二台机器进得来。[08-security §3.1](../design/08-security.md) 早已
定下形态——多节点走 mTLS，bootstrap token 换每节点证书——但没定三件事：

1. token 换证书的那一刻，**要不要有人点头**
2. 一条 RPC 到达时，**节点身份以什么为准**
3. 一张证书作废之后，**怎么让它失效**

## 候选

### ① 加入是否需要人工批准

| 项目 | 机制 | 代价 |
|---|---|---|
| Kubernetes | bootstrap token 换取提交 CSR 的权限，CSR 需被批准（可配置自动批准） | 两段式，概念多（CSR 对象、审批者、签名者） |
| Salt | minion 自报名字，master 端 `salt-key -a` 人工接受 | 接受之前 minion 一直轮询；名字可被抢占 |
| Puppet | 同上，`autosign` 可按域名白名单自动签 | 白名单基于**名字**，冒名即可绕过 |
| Chef | 预先在 server 上建 client 并下发 validator key | validator key 是**长期**共享凭据，泄露即全网可加入 |
| Nomad / Consul | ACL token 加入即生效（Consul `auto_encrypt` 用它换 TLS 证书） | 无人工确认环节 |

所有做法都是「先有一个凭据，再换一份长期身份」。分歧只在换取的那一刻
要不要有人点头。

### ② 身份来源

`RegisterRequest` 里有 `node_name`（[17-protocol §2](../design/17-protocol.md)），
那是单机形态留下的——unix socket 上没有证书，只能自报。

### ③ 吊销

| 做法 | 代价 |
|---|---|
| 标准 CRL / OCSP | 需分发与刷新机制、有缓存窗口（吊销不立即生效） |
| mechd 应用层状态检查 | 不是标准 PKI 姿态；多 mechd 时状态要同步 |

## 决策

**① token 即授权，不设人工批准环节。**

```bash
mechctl node token create --node store-042 --ttl 30m   # 绑名
mechctl node token create --ttl 30m --uses 20          # 批量预置，名字先到先得
mechctl node token list / revoke <id>
```

token 是**带外交付**的——运维自己拿到手上再敲进那台机器——那次交付本身
就是点头。冒名用一条更强的约束堵住：**token 可绑定节点名**，绑名的 token
只能签出该 CN 的证书；不绑名的可自报名字，但**不能顶替一个已签发过证书的
名字**（那要先 `node remove`）。

**② 身份以客户端证书的 CN 为准，不认请求里的 `node_name`。**

| 监听 | 身份来源 |
|---|---|
| unix socket | 隐式（socket 权限 `0600 root:root`），用请求里的 `node_name` |
| TCP + mTLS | 证书 CN；**忽略** `node_name`，不一致时**拒绝** |

不一致时拒绝而非静默以证书为准：静默会让一次配置错误（改了 `--node` 却
没换证书）表现成「节点名莫名其妙变回去了」。

**③ 吊销走 mechd 应用层状态检查，不做 CRL。**

握手照常成功，但每个 RPC 都查一次 CN 对应的节点是否仍有效；
`node remove` / `node revoke` 之后该证书的所有 RPC 被拒，并在审计里留下
「已吊销的节点尝试连接」。

## 后果

**收益**

- 加一台机器只需一条命令，批量预置（cloud-init / 镜像 / kickstart）走得通
  ——这正是边缘门店场景的主要形态
- 没有 pending 状态、没有审批者概念、没有第二条命令，实现与心智都小一圈
- 吊销**立即生效**，没有 CRL 的缓存窗口
- 复用 `mechdcmd/tls.go` 已有的 CA，不新写一套 PKI

**代价**

- **拿到未绑名 token 的人可以往集群里塞一台机器。** TTL、使用次数上限、
  可吊销、每次使用进审计是全部的限制手段——没有第二道人工闸门。
  安全姿态因此弱于 Kubernetes / Salt / Puppet。
- **应用层吊销不是标准 PKI 姿态。** 一张被吊销的证书**仍能完成 TLS 握手**，
  只是之后每个 RPC 被拒。任何依赖「握手成功即已授权」的中间件（反向代理、
  L7 网关）在这里都会得到错误的结论。
- 上一条成立的前提是 [ADR-0014](0014-no-ha-in-v1.md)：**只有一个 mechd**
  持有全部状态，CRL 要解决的「校验方与签发方分离」不存在。将来真要多
  mechd，吊销状态得跟着一起同步。
- `node_name` 字段在两种传输下含义不同（一处可信、一处必须忽略）。这是
  单机与多机共用一套协议的直接代价，必须在**一处**收口，否则会长出
  「某个 RPC 忘了忽略它」这类漏洞。
