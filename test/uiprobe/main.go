// uiprobe 从集群内部取一个 HTTPS 页面，打印状态码、类型与长度。
//
// 为什么要一个夹具：节点镜像里没有 curl、wget、python，集群端口也不对宿主
// 发布（test/multinode 的包注释说明了为什么不给被测机器装工具）。而 Web UI
// 的验收问题恰恰是「浏览器请求这个地址，拿回来的是什么」——那需要一次真的
// HTTP 往返，不是一次 TCP 连通性检查。
//
// 与 tlsprobe 的分工：tlsprobe 只做 TLS 握手，判的是「连接被不被接受」；
// uiprobe 走完整条 HTTP，判的是「响应体是不是那个页面」。
package main

import (
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	// -token 放在最前，其余位置参数不变：加一个可选开关不该动已有调用
	token := flag.String("token", "", "Bearer token（取 API 时用）")
	dump := flag.Bool("dump", false, "把响应体原样打出来")
	flag.Parse()
	args := flag.Args()
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr,
			"用法: uiprobe [-token T] [-dump] <url> <ca.crt> [期望的子串]")
		os.Exit(2)
	}
	url, caFile := args[0], args[1]

	pem, err := os.ReadFile(caFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读 CA:", err)
		os.Exit(1)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		fmt.Fprintln(os.Stderr, "CA 文件里没有证书:", caFile)
		os.Exit(1)
	}

	cli := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
		// 不跟随跳转：UI 的路由回退是 200 返回 index.html，若有人把它改成
		// 302，跟随之后看起来仍然「成功」——那正是要发现的变化
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "构造请求:", err)
		os.Exit(1)
	}
	if *token != "" {
		req.Header.Set("Authorization", "Bearer "+*token)
	}

	resp, err := cli.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "请求失败:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读响应体:", err)
		os.Exit(1)
	}

	fmt.Printf("HTTP %d  %s  %d 字节  编码=%s\n",
		resp.StatusCode,
		resp.Header.Get("Content-Type"),
		len(body),
		orDash(resp.Header.Get("Content-Encoding")))

	if *dump {
		fmt.Println(string(body))
	}
	if len(args) > 2 {
		want := args[2]
		if !strings.Contains(string(body), want) {
			fmt.Fprintf(os.Stderr, "响应体里没有 %q\n", want)
			os.Exit(1)
		}
		fmt.Printf("  含 %q ✓\n", want)
	}
	if resp.StatusCode != http.StatusOK {
		os.Exit(1)
	}
}

func orDash(s string) string {
	if s == "" {
		return "identity"
	}
	return s
}
