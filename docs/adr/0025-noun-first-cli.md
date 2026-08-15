# ADR-0025: CLI 采用名词优先结构

- **状态**：已接受
- **日期**：2026-08-02
- **相关**：[ADR-0003](0003-object-model-naming.md)

## 背景

M0 定稿的 CLI 动词表本身是不自洽的——一半动词优先、一半名词优先：

```bash
mechctl get components        ← 动词优先
mechctl deploy postgresql     ← 动词优先
mechctl node cordon n1        ← 名词优先
mechctl rollout pause x       ← 名词优先
mechctl config group list     ← 名词优先
```

这不是风格问题，是结构没定。M2 开始要往 CLI 里加大量命令，必须先定死。

## 问题的实际规模

把全部操作列出来数：

| 名词 | 操作数 | 其中**只对该名词成立**的动词 |
|---|---|---|
| node | 10 | `bootstrap` `cordon` `uncordon` `drain` `facts` `adopt-path` |
| component | 14 | `deploy` `upgrade` `rollback` `assign` `set-profile` |
| config | 6 | `explain` `group` |
| rollout | 5 | `pause` `resume` `abort` |
| site / pack / orphans / context | 3+3+2+4 | `pull` `purge` `use` |

**8 个名词、约 47 个操作，其中 19 个动词只对单一名词成立。**

第三列是关键。动词优先意味着 `cordon` / `drain` / `pause` / `move` / `adopt-path` / `explain` 这些**全部要挤进顶层**。

## 候选方案与调研

### 方案 A：动词优先（kubectl 式）

```bash
kubectl get pods
kubectl delete deployment nginx
kubectl cordon node-1
kubectl rollout pause deploy/x
```

- ✅ `get` / `describe` / `delete` 在所有资源类型上统一
- ❌ **kubectl 自己就没做到纯粹**：`cordon` `drain` `taint` `label` `annotate` `rollout` `top` `scale` `expose` 全是只服务单一资源的顶层动词
- ❌ 顶层动词数量随资源类型的特殊操作线性增长，且无处可归

### 方案 B：名词优先（docker / gcloud / az 式）

```bash
docker container ls
docker image prune
gcloud compute instances list
az vm create
```

- ✅ 特殊操作有天然归属，顶层不膨胀
- ✅ **可发现性**：`mechctl component <TAB>` 直接列出能对组件做的全部事
- ❌ 高频命令变长

### 决定性证据：Docker 1.13 的迁移

Docker 在 2017 年把扁平命令重构成名词优先，旧形式降级为别名：

```
docker ps      →  docker container ls
docker images  →  docker image ls
docker rmi     →  docker image rm
```

**一个从扁平起步、长大后发现扁平撑不住的工具**——这比任何理论论证都有说服力。而 Mecharion 当前的名词数与 per-noun 操作数已经超过 Docker 当年重构时的规模。

## 决策

**采用名词优先：`mechctl <名词> <动词> [参数]`。**

### 无缩写别名，无复数别名

每个名词只有一种写法。不提供 `co` / `comp` / `si` / `no` 这类缩写，也不接受复数形式。

理由：**输入长度是补全该解决的问题，不该靠让用户记住 `co` 是 component 还是 config。** 缩写表本身是一份要背的东西，而补全是零记忆成本的。代价是补全必须做到位——因此它被列为一等功能而非附属品（[10-cli.md §9](../design/10-cli.md#9-补全)）。

### 顶层别名的准入规则

> **只有零歧义的动词才能做顶层别名。** 一个动词若对多个名词都成立、且参数类型相同，就必须留在名词之下。

顶层最终只有四个命令：

| 命令 | 为什么够格 |
|---|---|
| `apply -f` | 不属于任何名词——作用于一份可能同时含 Site / Component / ConfigGroup 的声明文件 |
| `deploy <pack>` | 只有 Component 能被 deploy，且参数是 **Pack 名**，与任何其他命令的参数类型都不同 |
| `version` / `completion` | 惯例 |

被这条规则挡住的（**故意不做别名**）：`status`（与 `rollout status` 撞）、`diff`（与 `config diff` 撞）、`logs` / `exec`（与 `node logs` / `node exec` 撞）、`list` / `show` / `remove`（对几乎所有名词成立）。

### `delete` 取消，只保留 `remove`

原设计有 `mechctl delete <kind> <name>` 与 `mechctl remove <component>` 两个动词，靠「是否触碰数据面」区分。名词优先之后**这个区分不再必要**——`component remove` 与 `site remove` 天然不会混淆，因为名词已经说清了作用对象。

一个动词比两个好，尤其当那两个的区别需要靠文档解释时。

## 理由

**核心理由：`remove` 的歧义必须由结构消解，而不是由约定。**

顶层的 `mechctl remove pg-main` 无法判断 `pg-main` 是 Component 还是 Node 还是 Site。任何靠「约定俗成」或「按参数猜」来消歧的方案，都是在把一个可以被结构彻底消除的误删风险留给用户。

**误删的代价远高于多打几个字符。** 这也是 `list` 不给顶层别名的原因——若 `list` 破例，`remove` 凭什么不破例？规则一旦有例外就不再是规则。

**次要理由：特殊操作有归属。** `cordon` / `drain` / `adopt-path` / `explain` / `move` 这类动词，在动词优先下要么污染顶层，要么被迫改名成通用词而失去精确性。名词优先让它们各归其位。

## 后果

### 收益

- 顶层保持 4 个命令 + 8 个名词，不随功能增长膨胀
- `remove` 的误删歧义在结构层面消失
- 特殊操作有天然归属，不必为了「能放进顶层」而改名
- `mechctl <名词> <TAB>` 是真实可用的探索入口
- 一个 `remove` 代替 `delete` + `remove` 两个动词

### 代价

- **高频命令变长**：`mechctl component list` vs `mechctl get components`。有意接受
- **补全成为硬依赖**：名词优先的可发现性收益全靠补全兑现。补全必须支持动态查询（Component 名、Node 名、Pack 名、profile 名、参数名），且要覆盖 bash / zsh / fish / powershell。这是实打实的额外工作量
- **`deploy` 是唯一的动词别名，形式上与 `component remove` 不对称**。这是刻意的：它通过了零歧义规则，且是新用户的第一条命令。文档中明示规范形式是 `component deploy`
- **来自 kubectl 的用户需要适应**：`kubectl get pods` 的肌肉记忆在这里不成立。文档中给一张对照表缓解
- **`drain` 的语义与 Kubernetes 不同**：Mecharion 没有调度器，`drain` 不会把实例迁到别的节点。必须在文档中显式说明，否则会有错误预期

## 参考

- Docker 1.13 命令重构（2017）——从扁平迁移到名词优先的实例
- kubectl 的顶层动词膨胀：`cordon` `drain` `taint` `label` `annotate` `rollout` `top` `scale` `expose`
- gcloud / az 的名词优先层级
- Terraform 的极简动词表（`plan` / `apply` / `destroy`）——另一种解法：把名词全部收进配置文件，CLI 只留三个动词。Mecharion 因为需要大量运维态操作（cordon / drain / exec / logs）而无法采用
