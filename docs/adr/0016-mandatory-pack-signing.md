# ADR-0016: Pack 签名为必需项

- **状态**：已被 [ADR-0040](0040-pack-trust-is-operator-responsibility.md) 取代
- **日期**：2026-08-01
- **相关**：[ADR-0015](0015-offline-first-hermetic.md)、[ADR-0005](0005-pack-logic-payload-split.md)

## 背景

**mechlet 必然以 root 运行**——它要管理 systemd unit、创建系统用户、写 `/etc`、挂载文件系统。因此：

> **安装一个 Pack ≡ 在目标机器上执行 root 代码。**

同时，[ADR-0015](0015-offline-first-hermetic.md) 确立了离线优先：Pack 通过 U 盘、内网文件服务器、跨网闸摆渡等通道传递。

## 候选方案与调研

### 方案 A：签名为可选特性（多数工具的做法）

| 系统 | 现状 |
|---|---|
| **Helm** | 支持 provenance 签名，但**默认不校验**，绝大多数 Chart 未签名 |
| **Ansible Galaxy** | 早期无签名，后期加入但采用率低 |
| **Docker Content Trust** | 存在多年，默认关闭，实际采用率极低 |
| **APT / YUM** | **默认强制校验**，未签名包需显式 `--allow-unauthenticated` |

**规律非常清楚：默认关闭的签名机制，实际采用率趋近于零。**

### 方案 B：沙箱化 Pack 执行

限制 Pack 能做的事，降低对信任的依赖。

- ❌ 与「必须以 root 完成系统级配置」根本矛盾——要建用户、写 /etc、控制 systemd 的东西无法被有意义地沙箱化
- ❌ **虚假的安全感比没有更危险**

否决。

### 方案 C：签名为必需项，默认 enforce ⭐

## 决策

**Pack 签名是必需项，默认 `enforce`。**

```
mechpack sign --key publisher.key
```

- 签名覆盖 pack.yaml、全部模板/文件/hooks，以及**每个 blob 的 sha256**
- 节点侧维护可信发布者公钥列表：`/etc/mecharion/trust/`
- mechlet 在物化前校验签名 + 逐个校验 blob 摘要，任一失败则**拒绝执行**

```yaml
# /etc/mecharion/mechlet.yaml
security:
  packVerification: enforce      # enforce | warn | disabled
```

`disabled` 仅供本地开发，且 mechlet 启动时会**持续告警**——不能让「临时关掉」悄悄变成生产状态。

## 理由

### ① 离线场景**提高**了签名的必要性，而非降低

这一点常被搞反。对比：

| | 在线场景 | 离线场景 |
|---|---|---|
| 传输通道 | HTTPS | U 盘 / 文件服务器 / 网闸摆渡 |
| 内容信任兜底 | registry 的 digest 校验 | **无** |
| 中间人风险 | 低 | 高（物理介质可被替换） |

**离线时签名是唯一的信任锚**，没有任何其他机制兜底。

### ② 信任边界在「谁做的」，而不是「能做什么」

既然沙箱化不可行（方案 B），安全设计的重心只能放在**内容来源可信**上。这不是退而求其次，而是对这类工具唯一有意义的安全模型——APT/YUM 二十年来采用的正是这个模型。

### ③ 默认必须是 enforce

Helm、Docker Content Trust 的历史证明：**默认关闭 = 实际不用**。而一旦生态中大量 Pack 未签名，再改默认值就是破坏性变更，改不动了。

必须在第一天就 enforce。

## 后果

### 收益

- 离线分发有可靠的信任锚
- blob 摘要校验同时提供完整性保证（与内容寻址天然衔接）
- 与 APT/YUM 用户的心智模型一致，无需教育
- 生态从第一天就是签名的，不存在后期迁移问题

### 代价

- **开发体验摩擦**：本地迭代 Pack 时每次都要签名。缓解：`mechpack assemble` 支持开发密钥自动签名；`--dev` 模式配合 `packVerification: warn`
- **密钥管理成为必须**：项目需维护官方签名密钥、轮换流程、泄露应急预案。这是真实的运维负担
- **第三方 Pack 需要建立信任流程**：用户必须主动把发布者公钥加入 `trust/`。这是有意的摩擦——安装第三方 root 代码本来就该是一个自觉的决定
- **公钥分发问题**：官方公钥随 mechlet 二进制分发（信任根随引导建立）；第三方公钥由用户自行获取并验证指纹
- **不防御恶意的可信发布者**：签名只保证「这确实是 X 做的」，不保证「X 是好的」。这是签名机制的固有边界，需在文档中说清楚

## 参考

- APT / YUM 的默认强制签名校验
- Helm provenance（默认不校验的反面案例）
- Docker Content Trust 的实际采用率
- Sigstore / cosign（可作为后续的签名后端选项）
