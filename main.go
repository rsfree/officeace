// officeace2api 把 OfficeAce 桌面客户端的登录态包装成 OpenAI / Anthropic 兼容 API，
// 并提供账号池与 Web 管理面板。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"officeace2api/internal/account"
	"officeace2api/internal/config"
	"officeace2api/internal/server"
)

// version 由发布流水线通过 -ldflags "-X main.version=..." 注入。
var version = "dev"

func main() {
	dataDir := flag.String("data", "data", "数据目录（config.json / accounts.json）")
	listen := flag.String("listen", "", "覆盖监听地址，如 127.0.0.1:8787")
	flag.Parse()

	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		log.Fatalf("创建数据目录失败: %v", err)
	}
	cfgPath := filepath.Join(*dataDir, "config.json")
	cfg := config.Load(cfgPath)
	pool := account.NewPool(filepath.Join(*dataDir, "accounts.json"))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server.Version = version
	srv := server.New(cfg, pool)
	srv.StartLocalSync(ctx)

	c := cfg.Get()
	addr := c.Listen
	if *listen != "" {
		addr = *listen
	}

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}

	printBanner(addr, c.PanelKey, c.APIKey, c.UpstreamBase, len(pool.List()))

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		log.Fatalf("服务启动失败: %v", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	log.Println("已退出")
}

func printBanner(addr, panelKey, apiKey, upstream string, accounts int) {
	fmt.Printf(`
  officeace2api  ·  OfficeAce → OpenAI 兼容网关

  面板      http://%s/
  面板密钥  %s
  API Key   %s
  上游      %s
  账号数    %d
  版本      %s

  对话      POST http://%s/v1/chat/completions
  模型      GET  http://%s/v1/models
  图片      POST http://%s/v1/images/generations
  Claude    POST http://%s/v1/messages

`, addr, panelKey, apiKey, upstream, accounts, version, addr, addr, addr, addr)
}
