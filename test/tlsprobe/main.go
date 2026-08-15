// Command tlsprobe 拿一个**不带客户端证书**的连接去撞一个 TLS 端口，
// 报告握手成没成功。
//
// 它只为一条验收存在：**mechd 的 gRPC 监听必须要求客户端证书**
// （22-multi-node §5 第 5 行）。那一条原本测的是「agent 没有证书时自己
// 拒绝启动」——而一个根本没启用 mTLS 的 mechd 照样能让它通过，因为客户端
// 压根没走到握手。要验服务端，就得真的去握一次手。
//
// 为什么是个夹具二进制而不是 `openssl s_client`：被测镜像里没有 openssl，
// 而集群的端口不对宿主发布。**验收需要的能力用夹具补，不往被测镜像里装
// 工具**——装了它就不再是那台被测机器了。这与 test/webapp 是同一类东西。
package main

import (
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"time"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "用法: tlsprobe <host:port>")
		os.Exit(2)
	}
	conn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 10 * time.Second},
		"tcp", os.Args[1],
		// **只跳过服务端校验**：这个探针不关心 mechd 的证书对不对，
		// 它要问的是「服务端认不认没有客户端证书的人」。
		&tls.Config{InsecureSkipVerify: true}, //nolint:gosec // 见上
	)
	if err != nil {
		fmt.Printf("握手失败: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	// **握手成功还不够**：TLS 1.3 下客户端这一侧的握手会先「完成」，
	// 服务端「缺客户端证书」那个告警要到下一次读写才回来。不动一下的话，
	// 一个不要求证书的服务端与一个要求证书的服务端在这里长得一模一样。
	//
	// **先写再读**：光读是不行的——HTTP 服务端在收到请求之前不会说话，
	// 于是一条完全正常的连接也会读到超时或 EOF。发一个最小请求，
	// 让对端有理由回话；回什么不重要（200 还是 400 都算「这条连接能用」）。
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte("GET / HTTP/1.0\r\n\r\n")); err != nil {
		fmt.Printf("握手后被拒（写）: %v\n", err)
		os.Exit(1)
	}
	if _, err := conn.Read(make([]byte, 1)); err != nil {
		fmt.Printf("握手后被拒（读）: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("握手成功且服务端接受了这条无证书连接")
}
