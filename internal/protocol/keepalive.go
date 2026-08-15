package protocol

import (
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

// 拔掉电源不产生 TCP FIN。
//
// mechlet 挂着一条长期不说话的 Subscribe 流；机器断电之后，服务端那一侧
// 会一直等下去——Linux 默认的 TCP keepalive 要 **2 小时**才开始探测。在此
// 之前 Server.Connected 会一口咬定那台机器在线，而节点状态正是拿它算的
// （22-multi-node §6.13）。因此探测必须在应用层做。
const (
	// KeepaliveTime 是服务端没看到流量多久之后发一次探测。
	//
	// 与 ReconcileSeconds（60s）同量级：比它短没有意义，比它长会让
	// 「机器没了」比「机器没在调和」还晚被发现。
	KeepaliveTime = 60 * time.Second
	// KeepaliveTimeout 是探测之后等回应的时间。超过即判连接已死。
	//
	// 上限 KeepaliveTime + KeepaliveTimeout = 80s 发现断电。
	KeepaliveTimeout = 20 * time.Second
)

// ServerKeepalive 是 mechd 侧的 keepalive 选项。
//
// 两半都要：Params 让服务端**主动**探测（发现断电的那一半），
// EnforcementPolicy 决定服务端**容忍**客户端探测得多勤。
//
// 后者是个坑：gRPC 的 `MinTime` 默认 **5 分钟**，而客户端配 60s 探测时
// 服务端会直接把它踢掉，症状是连接每隔几分钟断一次、日志里只有一句
// `too_many_pings`——看上去像网络问题，实则是两侧策略没对上。
// PermitWithoutStream 同理：Subscribe 流之外的空闲期客户端仍要探测。
func ServerKeepalive() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    KeepaliveTime,
			Timeout: KeepaliveTimeout,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			// 比客户端的 KeepaliveTime 略松，留出时钟与调度的余量
			MinTime:             KeepaliveTime / 2,
			PermitWithoutStream: true,
		}),
	}
}

// ClientKeepalive 是 mechlet 侧的 keepalive 选项。
//
// 客户端也要探测：服务端进程没了而机器还在时，节点得自己发现并重连，
// 否则它会抱着一条死连接一直等下发。
func ClientKeepalive() grpc.DialOption {
	return grpc.WithKeepaliveParams(keepalive.ClientParameters{
		Time:                KeepaliveTime,
		Timeout:             KeepaliveTimeout,
		PermitWithoutStream: true,
	})
}
