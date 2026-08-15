package state

import (
	"path/filepath"
	"sort"
	"time"
)

const fileGarbage = "garbage.json"

// Garbage 是节点级的回收清单。
//
// **为什么是状态而不是事件。** 一代 generation 被回收之后，它引用的镜像与
// 载荷才成为回收候选。如果把这件事当成「prune 返回一个列表、调用方顺手
// 删掉」，那么进程在这两步之间重启一次，这些东西就**永远**不会再被任何人
// 想起——磁盘上多出几百 MB，且没有任何线索指向它们。
//
// 这与 M5 那次的教训是同一条：状态可以重复确认，事件丢一次就永远丢了。
// 于是候选先落盘，删成功才划掉。
//
// 清单里的东西**不代表一定要删**：判据是全局的（同一个镜像可能还被别的
// 实例引用），GC 每次都拿它与全部实例的引用集比对一次。
type Garbage struct {
	SchemaVersion int `json:"schemaVersion"`
	// Images 是候选镜像引用，Blobs 是候选载荷摘要。
	Images []GarbageItem `json:"images,omitempty"`
	Blobs  []GarbageItem `json:"blobs,omitempty"`
}

// GarbageItem 是一条候选。
//
// 带上 Since 是为了让「删不掉的东西一直在清单里」这件事可诊断——
// 一条挂了三个月的记录说明某处有问题，而只有 ID 的话看不出来。
type GarbageItem struct {
	ID    string    `json:"id"`
	Since time.Time `json:"since"`
}

func (s *Store) garbagePath() string { return filepath.Join(s.root, fileGarbage) }

// LoadGarbage 读取回收清单；不存在时返回空清单而不是 nil，
// 调用方因此不必分辨「没有」与「读失败」。
func (s *Store) LoadGarbage() (*Garbage, error) {
	var g Garbage
	ok, err := readJSON(s.garbagePath(), &g)
	if err != nil {
		return nil, err
	}
	if !ok {
		return &Garbage{}, nil
	}
	return &g, nil
}

// SaveGarbage 原子写入回收清单。
func (s *Store) SaveGarbage(g *Garbage) error {
	g.SchemaVersion = SchemaVersion
	return writeJSON(s.garbagePath(), g)
}

// Add 把候选加进清单，已在其中的不重复添加也不刷新时间。
//
// 不刷新时间是刻意的：Since 要回答「它躺在这里多久了」，
// 每次调和都刷一次会让这个问题永远得到「刚刚」这个无用的答案。
func (g *Garbage) Add(now time.Time, images, blobs []string) {
	g.Images = addItems(g.Images, now, images)
	g.Blobs = addItems(g.Blobs, now, blobs)
}

func addItems(cur []GarbageItem, now time.Time, add []string) []GarbageItem {
	have := make(map[string]bool, len(cur))
	for _, it := range cur {
		have[it.ID] = true
	}
	for _, id := range add {
		if id == "" || have[id] {
			continue
		}
		have[id] = true
		cur = append(cur, GarbageItem{ID: id, Since: now})
	}
	sort.Slice(cur, func(i, j int) bool { return cur[i].ID < cur[j].ID })
	return cur
}

// Drop 从清单里移除若干条。
func (g *Garbage) Drop(images, blobs []string) {
	g.Images = dropItems(g.Images, images)
	g.Blobs = dropItems(g.Blobs, blobs)
}

func dropItems(cur []GarbageItem, drop []string) []GarbageItem {
	if len(drop) == 0 {
		return cur
	}
	gone := make(map[string]bool, len(drop))
	for _, id := range drop {
		gone[id] = true
	}
	out := cur[:0]
	for _, it := range cur {
		if !gone[it.ID] {
			out = append(out, it)
		}
	}
	return out
}

// LiveRefs 汇总全部实例的全部 generation 仍在引用的镜像与载荷。
//
// **判据必须是全局的**：一个载荷可以被多个实例、多个组件共用（内容寻址的
// 直接后果），镜像更是如此。单个实例的调和器回答不了「谁还在引用它」。
func (s *Store) LiveRefs() (images, blobs map[string]bool, err error) {
	keys, err := s.ListInstances()
	if err != nil {
		return nil, nil, err
	}
	images, blobs = map[string]bool{}, map[string]bool{}
	for _, k := range keys {
		in, err := s.LoadInstance(k)
		if err != nil {
			// 读不出来的实例**当成还在引用**：漏删一次只是没省下空间，
			// 误删一次要靠重新分发几百 MB 来补。
			return nil, nil, err
		}
		if in == nil {
			continue
		}
		for _, g := range in.Generations {
			for _, id := range g.Images {
				images[id] = true
			}
			for _, id := range g.Blobs {
				blobs[id] = true
			}
		}
	}
	return images, blobs, nil
}
