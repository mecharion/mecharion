// Package faults 定义操作失败的分类。
//
// 它被资源引擎与 Runtime 共用：Rollout 要据此决定「重试本批」还是
// 「直接暂停」，若两边各有一套分类，编排层就得先做一次翻译——而翻译
// 表迟早会漏掉一种情况。
//
// 见 docs/design/11-resource-engine.md §2.3。
package faults

import (
	"errors"
	"fmt"
)

// Class 是失败的可重试性。
//
// 把网络抖动和配置错误当成同一回事，会让运维在两种完全不同的故障上
// 采取相同的（错误的）动作。
type Class int

const (
	// Transient 可重试：超时、临时 IO 错误、资源忙、载荷还没下载完。
	Transient Class = iota
	// Permanent 不可重试：权限不足、路径非法、内核不支持、参数写错。
	Permanent
)

func (c Class) String() string {
	if c == Transient {
		return "transient"
	}
	return "permanent"
}

// Error 是带分类的错误。
type Error struct {
	Class Class
	// Op 是失败的操作，如「解压」「查询 passwd」。出现在错误信息最前面。
	Op  string
	Err error
}

func (e *Error) Error() string {
	if e.Op == "" {
		return e.Err.Error()
	}
	return e.Op + ": " + e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

// Transientf 标记一个可重试的错误。
func Transientf(op, format string, a ...any) error {
	return &Error{Class: Transient, Op: op, Err: fmt.Errorf(format, a...)}
}

// Permanentf 标记一个不可重试的错误。
func Permanentf(op, format string, a ...any) error {
	return &Error{Class: Permanent, Op: op, Err: fmt.Errorf(format, a...)}
}

// Wrap 给一个已有错误打上分类。err 为 nil 时返回 nil。
func Wrap(class Class, op string, err error) error {
	if err == nil {
		return nil
	}
	return &Error{Class: class, Op: op, Err: err}
}

// ClassOf 返回错误的分类。
//
// **未分类的错误按 Permanent 处理**：把未知错误当成可重试，会让一个
// 配置错误在调和循环里无限重试；当成不可重试则会暂停并让人来看。
// 后者的代价小得多。
func ClassOf(err error) Class {
	var e *Error
	if errors.As(err, &e) {
		return e.Class
	}
	return Permanent
}

// IsTransient 报告错误是否可重试。
func IsTransient(err error) bool { return ClassOf(err) == Transient }
