// ssereader 连一条 SSE 流并把收到的事件打出来。
//
// 为什么要一个夹具：验收表第 14 条要的是「Rollout 进行中，第 N/M 批
// 自动更新，不用手动刷新」——那是一个**时间维度**的判据，一次 curl
// 式的取页面验不了它。而节点镜像里没有 curl，宿主与集群之间也不通
// （test/multinode 的包注释说明了为什么不给被测机器装工具）。
//
// 它只做一件事：连上、读、打印带时间戳的摘要。判断留给人或脚本。
package main

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	token := flag.String("token", "", "Bearer token")
	seconds := flag.Int("seconds", 20, "读多久")
	flag.Parse()
	args := flag.Args()
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "用法: ssereader [-token T] [-seconds N] <url> <ca.crt>")
		os.Exit(2)
	}

	pem, err := os.ReadFile(args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "读 CA:", err)
		os.Exit(1)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem)

	req, _ := http.NewRequest("GET", args[0], nil)
	if *token != "" {
		req.Header.Set("Authorization", "Bearer "+*token)
	}
	req.Header.Set("Accept", "text/event-stream")

	cli := &http.Client{
		// **不设 Timeout**：SSE 的响应本来就没有尽头，设了会在整数秒
		// 之后断掉，而那看起来像服务端的问题
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
	}
	resp, err := cli.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "连接失败:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	fmt.Printf("HTTP %d  %s\n", resp.StatusCode, resp.Header.Get("Content-Type"))
	if resp.StatusCode != http.StatusOK {
		os.Exit(1)
	}

	deadline := time.Now().Add(time.Duration(*seconds) * time.Second)
	go func() {
		time.Sleep(time.Until(deadline))
		resp.Body.Close()
	}()

	start := time.Now()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	snapshots, pings := 0, 0
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == ": ping":
			pings++
			fmt.Printf("  [%5.1fs] 心跳\n", time.Since(start).Seconds())
		case strings.HasPrefix(line, "retry:"):
			fmt.Printf("  [%5.1fs] %s\n", time.Since(start).Seconds(), line)
		case strings.HasPrefix(line, "data: "):
			snapshots++
			body := strings.TrimPrefix(line, "data: ")
			fmt.Printf("  [%5.1fs] 快照 #%d  %d 字节  %s\n",
				time.Since(start).Seconds(), snapshots, len(body), summarize(body))
		}
	}
	fmt.Printf("共收到 %d 份快照、%d 次心跳\n", snapshots, pings)
}

// summarize 从快照里挑出最能说明「它动了没有」的几项。
func summarize(body string) string {
	var out []string
	for _, key := range []string{`"state":"`, `"batch":`, `"batches":`, `"converged":`} {
		if i := strings.Index(body, key); i >= 0 {
			rest := body[i+len(key):]
			end := strings.IndexAny(rest, `,"}`)
			if end < 0 {
				end = len(rest)
			}
			out = append(out, strings.Trim(key, `":`)+"="+rest[:end])
		}
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, " ")
}
