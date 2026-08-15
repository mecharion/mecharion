// webapp 是 M2 的端到端测试夹具：一个最小的 Go Web 应用。
//
// 选它而非 nginx 是刻意的（docs/design/25-roadmap.md）：nginx 会把开发
// 拖进发行版差异、包版本可用性等与核心设计无关的泥潭；这个应用是自己
// 完全掌控的最小闭环——单静态二进制、无外部依赖、可在贫瘠容器里跑。
//
// 它读一份 YAML 风格的极简配置（只认 `key: value`），暴露 /healthz，
// 并支持 SIGHUP 热加载日志级别——正好覆盖 Pack 声明的 reloadRequired 语义。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// config 是配置文件里我们关心的字段。
type config struct {
	Port     int
	LogLevel string
}

func main() {
	var configPath string
	flag.StringVar(&configPath, "config", "", "配置文件路径")
	flag.Parse()

	cfg, err := load(configPath)
	if err != nil {
		log.Fatalf("读取配置: %v", err)
	}

	// logLevel 用原子值持有，SIGHUP 时就地换掉——热加载不重启进程，
	// 这正是 Pack 里 execReload 想要的效果。
	var logLevel atomic.Value
	logLevel.Store(cfg.LogLevel)

	started := time.Now()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "mecharion go-webapp\nport=%d\nlogLevel=%s\nuptime=%s\n",
			cfg.Port, logLevel.Load(), time.Since(started).Round(time.Second))
	})

	srv := &http.Server{
		Addr:              ":" + strconv.Itoa(cfg.Port),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// 先监听再进入信号循环：监听失败要立刻退出并让 systemd 看到，
	// 而不是变成一个「起来了但没在服务」的僵尸。
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		log.Fatalf("监听 %s: %v", srv.Addr, err)
	}
	log.Printf("go-webapp 已启动，监听 %s，日志级别 %s", srv.Addr, cfg.LogLevel)

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			newCfg, err := load(configPath)
			if err != nil {
				log.Printf("热加载失败，沿用旧配置: %v", err)
				continue
			}
			logLevel.Store(newCfg.LogLevel)
			log.Printf("已热加载配置，日志级别 %s", newCfg.LogLevel)
		}
	}()

	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-term
		log.Println("收到终止信号，正在优雅停止")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("服务退出: %v", err)
	}
	log.Println("已停止")
}

// load 读配置。格式是 YAML 的一个极小子集——夹具不该为了读三个字段
// 就引入一个依赖。
func load(path string) (config, error) {
	cfg := config{Port: 8080, LogLevel: "info"}
	if path == "" {
		return cfg, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
		switch k {
		case "port":
			n, err := strconv.Atoi(v)
			if err != nil {
				return cfg, fmt.Errorf("port 不是数字: %q", v)
			}
			cfg.Port = n
		case "log_level":
			cfg.LogLevel = v
		}
	}
	return cfg, nil
}
