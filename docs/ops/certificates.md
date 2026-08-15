# 证书

Mecharion 用两套独立的证书体系，背景见 [08-security.md](../design/08-security.md#3-传输安全)：

- **mechd 的 HTTP 服务端证书**——`mechctl`/浏览器连 mechd 用的那个，单机与多节点都有
- **多节点的 mTLS 节点证书**——只有多节点形态才用，mechd 与 mechlet 之间双向认证（[ADR-0034](../adr/0034-node-join-and-identity.md)）

两套都是**自签**、**默认全自动轮换**，正常运行时不需要人管。这份文档讲的是它们各自的生命周期、以及自动化失效时该怎么办。

## HTTP 服务端证书

| 项 | 有效期 | 位置 |
|---|---|---|
| CA | 10 年 | `/etc/mecharion/pki/ca.crt` + `ca.key`（`0600 root:root`） |
| 服务端证书 | 1 年 | `/etc/mecharion/pki/server.crt` + `.key` |

首次启动 mechd 时自动生成，**剩余有效期 < 30 天时自动重新签发并热重载**（不需要重启进程）。主机 IP 变化（DHCP）时下一次轮换会自动纳入新地址。

### 客户端怎么信任这个 CA

| 场景 | 做法 |
|---|---|
| 本机 `mechctl` | 直接读 `/etc/mecharion/pki/ca.crt`，零配置 |
| 远程 `mechctl` | `mechd ca export --out ca.crt`（在 mechd 那台机器上跑），再 `mechctl context set <name> --ca-file ca.crt` |
| 浏览器 | 导入同一份 `ca.crt`，各操作系统的导入步骤见对应系统文档 |

### 故障排查：证书没有按预期轮换

自动轮换是"下次有请求进来时顺带检查"，不是一个独立的后台定时器——如果 mechd 长期没有收到任何 HTTP 请求，轮换检查也不会被触发。确认方法：

```bash
openssl x509 -in /etc/mecharion/pki/server.crt -noout -enddate
```

如果剩余有效期确实 < 30 天但没有轮换，检查 `journalctl -u mecharion-mechd` 里有没有写证书失败的报错（多半是 `/etc/mecharion/pki/` 目录权限问题——mechd 的运行用户对这个目录没有写权限）。

### 企业环境：外部证书

`http.tls.mode: provided` 可以指定企业 CA 或 Let's Encrypt 签发的证书；一旦这样配置，Mecharion **不再管理它的生命周期**——轮换、续期是运维方自己的责任，与上面这套自动化无关。

## 多节点 mTLS 节点证书

只有多节点形态才涉及。每个节点用 join token 换一张证书（`mechctl node token create`），**身份以证书 CN 为准，不是请求里报的节点名**——这是 mTLS 挡住"一个节点冒充另一个节点"的关键（[ADR-0034](../adr/0034-node-join-and-identity.md)）。

**剩余有效期 < 30 天时自动轮换**，与服务端证书同一条机制。

### 离线签发

正常路径（join token）需要节点连得上 mechd。两种连不上的场景走离线路径：

```bash
# 在 mechd 那台机器上跑
mechd ca issue --node <节点名> --out-dir /tmp/<节点名> [--validity 720h]
```

把 `/tmp/<节点名>` 整个目录拷到目标机器的 `<配置目录>/pki` 下即可——落地的三个文件名（`node.crt`/`node.key`/`ca.crt`）已经是目标机器期望的名字，不需要手工改名。

### 吊销

```bash
mechctl node revoke <节点名>     # 撤销
mechctl node unrevoke <节点名>   # 撤销这次撤销
```

**吊销走应用层状态检查，不做 CRL**——mechd 只有一个，判断"这张证书还作不作数"直接查数据库里的状态，不需要证书吊销列表这套机制。

**一个容易踩的坑**：一张被吊销的证书**仍然能完成 TLS 握手**，只是握手之后每个 RPC 都会被拒绝、并在审计里留一条。任何以"握手成功即已授权"为前提的中间件（反向代理、L7 网关）在这里都会得出错误结论——如果你在 mechd 前面放了反代，不要指望它替你挡掉被吊销的节点。

## 灾难场景：CA 私钥丢了

这是最严重的情况，如实说明代价：CA 私钥（`/etc/mecharion/pki/ca.key`）丢失或损坏后，**没有恢复手段**——它不在数据库备份范围内（数据库只有引用不含私钥本身），必须单独备份（见 [backup-and-restore.md](backup-and-restore.md)）。

没有那份单独备份时唯一的出路是：

1. 重新初始化整套 PKI（删掉 `/etc/mecharion/pki/`，重启 mechd 让它重新生成一套全新的自签 CA）
2. 全部现有的客户端信任（`mechctl` 的 `--ca-file`、浏览器导入的证书）随之失效，需要重新分发新的 `ca.crt`
3. 多节点形态下，全部节点证书也随之失效（它们是旧 CA 签的，新 CA 不认）——每个节点都要重新走一次 join 流程

这正是"PKI 要单独备份"在清单里被专门列出来的原因——如果只备份了数据库，CA 私钥丢失时数据库本身完好无损也救不了这个局面。
