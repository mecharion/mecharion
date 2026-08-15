# 故障排查

## 第一步：mechd 是活着，还是能干活

两个不同的问题，两个不同的端点（[REV-033](../review/20260809/06-defect-register-and-roadmap.md)）：

```bash
curl -k https://<mechd>:8443/api/v1/healthz   # 进程活着吗——无条件 200，不查任何依赖
curl -k https://<mechd>:8443/api/v1/readyz    # 真能处理请求吗——查数据库
```

`/healthz` 答 200 但 `/readyz` 答 503：进程本身没死，但数据库打不开或查询挂死——响应体的 `reason` 字段会给出具体原因。`/healthz` 本身不通：进程可能已经崩溃或没启动，去看 `journalctl`（下面）。

两者都**不需要认证**，也**不查 Pack、组件、节点这些业务状态**——它们只回答"mechd 这个进程/依赖是否健康"，不是"你部署的东西是否健康"（那是 `mechctl component status` 的职责）。

## mechd 完全连不上：`--local` 诊断

如果 `mechctl` 连不上 mechd（网络分区、mechd 本身挂了），在**目标机器上**可以绕开 mechd，直连本机 mechlet 的只读诊断入口：

```bash
mechctl --local component status
```

这条路径**只读**、**不认证**（unix socket 的文件权限就是认证——只有本机能碰到这个 socket），只能看"这台机器上部署了什么、状态如何"，看不到跨节点的信息，也不能做任何写操作。它存在的目的只有一个：mechd 不可达时还有办法看清本机的真实状况，不是常规操作入口。

## 日志

```bash
journalctl -u mecharion-mechd -f      # 控制面
journalctl -u mecharion-mechlet -f    # 本机 agent
```

日志是结构化的（`slog`，JSON 格式），关键字段找 `level`（`ERROR`/`WARN` 优先看）、`err`（原始错误）、`request_id`（一次 HTTP 请求内的全部日志能用它串起来，见 [08-security.md](../design/08-security.md)）。

## 组件状态与漂移

```bash
mechctl component status <name>            # 收敛没有、每个实例的健康状态
mechctl component diff <name>               # 漂移了什么（如果启用了漂移检测）
mechctl component rollout status <name>     # 卡在滚动升级的哪一批
mechctl component rollout history <name>
```

`status` 里 `converged: false` 说明还没达到期望状态——正常情况下这是暂时的（调和器还没来得及处理），持续不收敛时看具体哪个 `instance` 的 `result` 字段。

## 节点状态

```bash
mechctl node list
```

节点的几种非正常状态与含义：

| 状态 | 含义 | 能不能部署上去 |
|---|---|---|
| `pending` | 已预登记，还没人在上面跑过 join | 不能——先完成 join |
| `offline` | 曾经在线，现在收不到下发 | 不能——部署上去会得到一个永远不收敛的实例 |
| `cordoned` | 手动暂停了调和 | 不能——先 `uncordon` |
| 证书已吊销 | `mechctl node revoke` 过 | 不能——先 `unrevoke`，见 [certificates.md](certificates.md) |

## 孤儿：机器上有、控制面不认的东西

```bash
mechctl orphans list
```

组件被移除时默认保留数据（防止误删），保留下来的目录会在这里出现，是**预期行为**，不是故障——但长期不清理会占用磁盘。清理见 `mechctl orphans purge`（`10-cli.md` §4.7）。

## Pack 上传/部署失败

`mechctl pack upload` 校验很严格（hermetic lint、路径 containment、sha256 一致性），失败信息本身会说明具体原因。**被拒的包什么都不会留下**——校验在临时目录里做，通过了才进集合，因此"部分上传"不是一个需要排查的状态。

## Web UI 打不开 / 502 / 证书告警

浏览器报证书不可信：mechd 用的是自签 CA，需要先导入 `ca.crt`（见 [certificates.md](certificates.md#客户端怎么信任这个-ca)）——这不是故障，是自签证书的预期行为，Chrome/Firefox 都会在导入前显示警告页。

## 目前没有的

如实说明边界（[REV-033](../review/20260809/06-defect-register-and-roadmap.md) 的其余部分）：

- **没有 metrics（Prometheus 之类）**——目前只有日志与上面这几个查询命令，没有可抓取的指标端点。
- **没有一键诊断包**——遇到问题时目前要按这份文档手工收集 `journalctl` 输出、`component status`、`node list` 等信息，没有一条命令把它们打包成一份脱敏归档。
- **没有全局搜索/事件时间线视图**——`GET /events`（HTTP API）有历史事件，但没有 `mechctl` 命令或 Web UI 页面能查询它，目前只能通过日志追溯。

这几项留给了 D7 未做完的部分与 D5（详见仓库 `docs/dev/M10-boundary-and-contract/plan.md`），不是这份文档漏写。
