# ADR-0004: 顶层作用域采用 Site

- **状态**：已接受
- **日期**：2026-08-01
- **相关**：[ADR-0003](0003-object-model-naming.md)

## 背景

对象模型的顶层需要一个作用域对象。它是全套命名中争议最大的一个，因此单独决策。

先明确它必须承担的**七项职责**——这是筛选候选的依据，而非语感：

| | 职责 |
|---|---|
| R1 | **归属**：一个 Node 属于且仅属于一个 X |
| R2 | **命名空间**：Component 名在 X 内唯一（`pg-main` 可在多个 X 中重名） |
| R3 | **状态边界**：`apply` / `diff` / `rollout` 的作用域 |
| R4 | **权限边界**：RBAC 授权到 X 级 |
| R5 | **规模无关**：1 个节点与 300 个节点都自然 |
| R6 | **关系无关**：节点协作（Hadoop 集群）与完全不协作（50 台独立 web 机 / 5000 个边缘盒子）都自然 |
| R7 | **可隐式**：单机安装后不应要求用户先手工创建一个 X |

**R5 与 R6 是真正的筛子。** R6 尤其关键：这个词**不能对节点之间的关系做出承诺**，因为产品同时服务两种相反的拓扑。

## 候选方案与调研

| 候选 | R5 规模 | R6 关系 | 边缘契合 | 大数据契合 | 认知成本 | 冲突风险 |
|---|---|---|---|---|---|---|
| **Cluster** | ❌ 1 节点别扭 | ❌ **断言节点协作** | ❌ 5000 个「单节点集群」 | ✅✅ | 最低 | Site 内会真的部署 Hadoop cluster → 套娃 |
| **Site** | ✅ | ✅ | ✅✅ | ⚠️ 需一句话解释 | 低 | 低 |
| **Fleet** | ⚠️ | ❌ 断言「大量同质单元」 | ✅✅ | ❌ 20 台异构机不叫 fleet | 低 | Rancher Fleet |
| **Environment** | ✅ | ✅ | ❌ 5000 个「环境」荒谬 | ⚠️ | 低 | 强绑 dev/staging/prod；用户还想拿它当标签 |
| **Datacenter** | ❌ | ✅ | ❌ 门店不是机房 | ✅ | 低 | Consul / Nomad |
| **Realm** | ✅ | ✅ | ✅ | ❌ | 中 | **Kerberos realm——Hadoop 场景正面冲突** |
| **Estate** | ✅ | ✅ | ✅ | ✅ | 高（英式用法） | 暗示「全部资产」而非子集 |
| **Zone / Region** | ✅ | ✅ | ⚠️ | ⚠️ | 低 | 云 AZ 概念冲突严重 |
| **Stack** | ✅ | ✅ | ✅ | ❌ | 中 | CloudFormation / Pulumi / Docker Stack / **Ambari Stack** |

### 边缘领域的既有实践

| 产品 | 顶层分组词 |
|---|---|
| **Balena** | **Fleet**（2021 年由 Application 改名而来） |
| **AWS Greengrass** | Thing Group |
| **Azure IoT Edge** | Deployment（按 tag 选设备） |
| **Portainer** | Environment |
| **Cisco SD-WAN / SASE** | **Site** |
| **Puppet Enterprise** | Node Group / Environment |
| **Rancher / k3s** | Cluster |

### `Site` 的既有基础设施先例

- **Active Directory Sites and Services** — 一组网络连通性良好的机器，与本项目的定义几乎相同
- **Cisco SD-WAN** — 一个门店/分支即一个 site，标准术语
- **工业 / SCADA / 电力** — 变电站、产线称为 site
- **VMware SRM** — protected site / recovery site

这些先例恰好覆盖了本产品边缘场景的来源行业。

### 第四条路：不要这个对象（评估后否决）

Ansible 与 Salt 都没有这一层，Node 只有 labels，Component 用 label selector 选目标。

- ✅ 命名问题彻底消失，模型最简，targeting 最灵活
- ❌ Component 名全局唯一 → 跨环境重名冲突
- ❌ 没有 RBAC 边界（「这个团队只能管这批机器」无法表达）
- ❌ **GUI 没有天然的顶层树**，只能给平铺列表 + 过滤器

第三条是决定性的：可视化界面是本产品的一等入口，而 Cloudera Manager 的可用性很大程度来自左侧的 Cluster 树。**有 GUI 就需要层级。**

## 决策

**采用 `Site`。**

配套引入 `kind` 字段承担分类语义：

```yaml
site:
  name: store-shanghai-0871
  kind: edge          # edge | cluster | standalone
  labels: { region: east, env: prod }
```

- `kind: edge` → UI 用网格/地图视图渲染上千个
- `kind: cluster` → UI 用服务拓扑视图
- `kind: standalone` → `mechlet install --standalone` 时由 mechd 隐式创建（满足 R7，用户从不需要显式建站点）

## 理由

**决定性理由是 R6：`Site` 是全部候选中唯一对节点关系零承诺的词。** 它只说「这些机器被一起管理」，不说它们互相通信、不说它们同质、不说它们数量多——这恰好是所需的全部语义。

与 `Cluster` 对比存在一个关键的**不对称性**：

- `Site` 在「同机房的 staging 与 prod 分属两个 Site」这类场景下只是**不够精确**
- `Cluster` 在「5000 个互不相关的边缘盒子」场景下是**说错了**——那些机器根本不构成集群

不够精确可以接受，说错不行。

此外，`kind` 字段让 `Site` 这个词只承担「管理分组」一个语义，「它们是不是集群/是不是边缘」由 `kind` 表达。**名字不必一词多担**，这也是不把语义压进名字本身的原因。

## 后果

### 收益

- 规模与关系无关，两端场景都自然
- 边缘场景的行业术语契合度高
- `kind` 驱动差异化 UI 呈现，成本极低
- `standalone` 使单机模式无需用户显式建对象

### 代价

- **中文「站点」有歧义**：Web 语境中 站点 ≈ 网站，中文开发者关联较强。缓解：文档统一写 `Site（站点）`，首次出现给定义；大数据语境的文档补一句「一个 Site 通常对应一个集群」。CLI 与 UI 中出现的是英文 `site`，歧义仅存在于中文散文，可控。
- **暗示物理位置**：同机房的 staging/prod 分属两 Site 略怪。属「不够精确」而非错误，接受。
- **大数据用户需要一次概念映射**：他们预期看到 `Cluster`。文档中显式说明映射关系。

### 重新评估的条件

若后续判断目标用户压倒性来自 CDH/Ambari 存量迁移人群，应重新评估 `Cluster`（届时写新 ADR 取代本文）。当前产品边界覆盖 JDK、nginx、PostgreSQL、Web 应用、主机配置与边缘单机，大数据只是其中一类，不满足该条件。

## 参考

- Active Directory Sites and Services
- Cisco SD-WAN site 概念
- Balena：Application → Fleet 改名（2021）
- Rancher Fleet / Portainer Environment / Consul Datacenter
- Ansible / Salt 的无顶层分组模型
