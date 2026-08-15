package mechd

import (
	"crypto/sha256"
	"encoding/binary"

	"github.com/mecharion/mecharion/internal/pack"
	"github.com/mecharion/mecharion/internal/render"
)

// previewSecrets 是给「先看看会发生什么」用的密钥视图：**读真的，写假的**。
//
// 它解决两个互相拉扯的要求：
//
//	① 预览不能有副作用。用真 Vault 渲染一遍，一个用户还没确认的新口令
//	   就已经落库了——版本号自增、digest 永久改变，即使他随后点了取消。
//	② 预览必须与真实渲染算出可比的 digest。dryRun 那条路走的
//	   ephemeralSecrets 每次生成一个新随机值，于是**每次渲染的 digest
//	   都不同**——「改了什么」的对比结果会是「全都改了」，等于没有对比。
//
// 做法：已有的密钥照实读（Vault 的 Ensure 本来就是「仅首次生成」，
// 对一个已部署的组件就是纯读）；用户这次新给的值不落库，换成一个
// **由值本身决定的**占位版本号。
//
// 占位版本号必须是值的函数，不能是随机数：把口令改回原来那个时 digest
// 应当不变，而随机数会把那也报成一次变更。
//
// **同一个类型同时用于基线渲染与预览渲染**，不能是两个前缀不同的实现：
// `SecretRefs` 参与 digest（spec/digest.go），而 SecretRef 里带着 id——
// 两个 id 前缀不同的实现会让每个实例都被误报成「变了」，而那正是这段
// 代码要消除的假象。
type previewSecrets struct{ real render.SecretStore }

var _ render.SecretStore = previewSecrets{}

func (p previewSecrets) Ensure(
	component, param string, g pack.Generate,
) (render.StoredSecret, error) {
	// generate 的密钥对已部署组件是纯读。真要缺一个，让它生成也无妨——
	// 「仅首次生成」保证随后的真实渲染拿到同一个值。
	return p.real.Ensure(component, param, g)
}

func (p previewSecrets) Store(
	component, param, value string,
) (render.StoredSecret, error) {
	sum := sha256.Sum256([]byte(param + "\x00" + value))
	return render.StoredSecret{
		ID:    "preview." + param,
		Value: value,
		// 取摘要前 4 字节当版本号：值变则版本变、digest 变；
		// 值不变则处处不变。**版本号是非负的**——它会进 JSON 与 SQLite，
		// 一个负数在那两处都只会引起困惑。
		Version: int(binary.BigEndian.Uint32(sum[:4]) >> 1),
	}, nil
}
