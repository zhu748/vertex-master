package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"
	"unicode"

	"github.com/bsfdsagfadg/vertex/internal/api"
	"github.com/bsfdsagfadg/vertex/internal/cli"
	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/db"
	"github.com/bsfdsagfadg/vertex/internal/logger"
	"github.com/bsfdsagfadg/vertex/internal/nodes"
	"github.com/bsfdsagfadg/vertex/internal/spool"
	"github.com/bsfdsagfadg/vertex/internal/statefile"
	"github.com/bsfdsagfadg/vertex/internal/transport"
	"github.com/bsfdsagfadg/vertex/internal/vertex"
)

var (
	//nolint:gochecknoglobals // Injected by build script
	version = "dev"
	//nolint:gochecknoglobals // Injected by build script
	buildCommit = "unknown"
	//nolint:gochecknoglobals // Injected by build script
	buildTime = "unknown"
)

//go:embed rules.txt
//nolint:gochecknoglobals // Embedded file
var rulesText string

const (
	shutdownGrace = 25 * time.Second
)

func rulesAgreedPath() string {
	return filepath.Join(config.ConfigDir(), "state", ".rules_agreed")
}

func rulesAgreedDockerPath() string {
	return filepath.Join(config.ConfigDir(), "state", "agreed-rules-docker.txt")
}

func rulesHash() string {
	cleanText := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, rulesText)
	sum := sha256.Sum256([]byte(cleanText))
	return hex.EncodeToString(sum[:])[:16]
}

func inDocker() bool {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("RENDER")), "true") {
		return true
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	if data, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		s := string(data)
		if strings.Contains(s, "docker") || strings.Contains(s, "containerd") || strings.Contains(s, "kubepods") {
			return true
		}
	}
	return false
}

// 提取原有的终端普通打印，仅在需要同意规则阶段展示
func printLegacyBanner() {
	fmt.Println("╔══════════════════════════════════════════════════════════════╗")
	fmt.Printf("║  Vertex AI Proxy  %-42s ║\n", version)
	fmt.Println("║  PolyForm Noncommercial License 1.0.0   Deconstructed_Cube   ║")
	fmt.Printf("║  Build: %s / %s                                  ║\n", buildCommit, buildTime)
	fmt.Printf("║  Platform: %s/%s                                       ║\n", runtime.GOOS, runtime.GOARCH)
	fmt.Println("╚══════════════════════════════════════════════════════════════╝")

	fmt.Println()
	fmt.Println("  ╔══════════════════════════════════════════════════════════╗")
	fmt.Println("  ║                                                          ║")
	fmt.Println("  ║   ⚠️  本软件完全免费，如果你花钱购买了这个软件，         ║")
	fmt.Println("  ║       你被骗了。请立即联系卖家退款。                     ║")
	fmt.Println("  ║                                                          ║")
	fmt.Println("  ║   获取正版：https://discord.gg/odysseia                  ║")
	fmt.Println("  ║                                                          ║")
	fmt.Println("  ╚══════════════════════════════════════════════════════════╝")
	fmt.Println()
}

func main() {
	setupTermuxCerts() // 优先初始化 Termux 证书
	if renderCommit := strings.TrimSpace(os.Getenv("RENDER_GIT_COMMIT")); buildCommit == "unknown" && renderCommit != "" {
		buildCommit = renderCommit
	}

	logDir := filepath.Join(filepath.Dir(config.ConfigDir()), "logs")
	dailyLogger := logger.NewDailyLogger(logDir)
	defer func() { _ = dailyLogger.Close() }()

	// ---- 状态文件迁移 ----
	stateDir := filepath.Join(config.ConfigDir(), "state")
	for _, migration := range [][2]string{
		{"config/.rules_agreed", filepath.Join(stateDir, ".rules_agreed")},
		{"config/agreed-rules-docker.txt", filepath.Join(stateDir, "agreed-rules-docker.txt")},
	} {
		if err := statefile.Migrate(migration[0], migration[1]); err != nil {
			log.Printf("[State] 迁移旧状态文件失败: %v", err)
		}
	}

	// ---- 规则同意检查 ----
	curHash := rulesHash()
	if inDocker() {
		if !checkRulesAgreedDocker(curHash) {
			printLegacyBanner()
			fmt.Println()
			fmt.Println("  ╔══════════════════════════════════════════════════════════╗")
			fmt.Println("  ║  📦 检测到 Docker 环境                                   ║")
			fmt.Println("  ╚══════════════════════════════════════════════════════════╝")
			fmt.Println()
			fmt.Println("  Docker 容器中无法交互同意规则。请按以下步骤同意：")
			fmt.Println()
			fmt.Println("  1) 在数据目录中创建规则同意文件：")
			fmt.Printf("       %s\n", rulesAgreedDockerPath())
			fmt.Println()
			fmt.Println("  2) 文件内容写入当前规则版本哈希（必须完全匹配）：")
			fmt.Printf("       %s\n", curHash)
			fmt.Println()
			fmt.Println("     一行命令：")
			fmt.Printf("       echo %s > %s\n", curHash, rulesAgreedDockerPath())
			fmt.Println()
			fmt.Println("     Render 部署可改为设置环境变量：")
			fmt.Printf("       VPROXY_RULES_HASH=%s\n", curHash)
			fmt.Println()
			fmt.Println("  3) 重启容器即可。")
			fmt.Println()
			os.Exit(0)
		}
	} else {
		if !checkRulesAgreed(curHash) {
			printLegacyBanner()
			fmt.Println(rulesText)
			fmt.Println()
			if hasOldAgreement() {
				fmt.Println("  ⚠️  规则已更新，需要您重新确认。")
				fmt.Println()
			}
			fmt.Print("  请输入 yes 同意以上规则（输入其他内容退出）：")
			reader := bufio.NewReader(os.Stdin)
			input, _ := reader.ReadString('\n')
			input = strings.TrimSpace(strings.ToLower(input))
			if input != "yes" {
				fmt.Println("  你未同意规则，程序退出。")
				os.Exit(0)
			}
			saveRulesAgreed(curHash)
			fmt.Println()
			fmt.Println("  ✓ 已同意规则，正在启动...")
			fmt.Println()
		}
	}

	// ---- 同意通过之后，干净地启动 TUI 看板，坚决不影响前面的交互输入 ───
	stopTracker := cli.InitTracker(dailyLogger)
	defer stopTracker()
	cli.SetAppInfo(version, buildCommit, buildTime, runtime.GOOS, runtime.GOARCH)

	cfg := config.GetProvider()
	if err := db.InitDB(filepath.Join(config.ConfigDir(), "data.db")); err != nil {
		log.Fatalf("[vproxy] failed to init database: %v", err)
	}
	defer db.CloseDB()

	spool.SetMaxSpillBytes(int64(cfg.MaxSpillMB()) << 20)

	nodes.DeleteNodeCallback = transport.RemoveProxy
	stopProxyGC := transport.StartProxyGC(5*time.Minute, 30*time.Minute)

	keys := api.NewAPIKeyManager()
	keys.LoadKeys()

	api.EnsureAdminPassword()
	stopAdminSessionCleanup := api.StartAdminSessionCleanup(time.Hour)

	if err := api.SyncEnvironmentProxySubscription(); err != nil {
		log.Printf("[ProxyPool] 同步环境变量代理订阅失败: %v", err)
	}

	vc := vertex.NewVertexAIClient(cfg)
	stopProxySubscriptionScheduler := api.StartProxySubscriptionScheduler(vc, cfg)
	stopProxyHealthScheduler := api.StartProxyHealthScheduler(vc, cfg)

	srv := api.NewServer(vc, keys, cfg)
	srv.SetBuildInfo(version, buildCommit, buildTime)
	//nolint:exhaustruct
	httpServer := &http.Server{
		Addr:              "0.0.0.0:" + strconv.Itoa(cfg.PortAPI()),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       180 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	shutdownDone := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		for s := range sig {
			if s == syscall.SIGHUP {
				config.InvalidateCache()
				config.InvalidateModelsCache()
				log.Printf("[vproxy] 收到 SIGHUP：已清配置/模型缓存，下次读取即热重载")
				continue
			}
			log.Printf("[vproxy] 收到 %v：开始关闭程序，等待在途请求处理完成(最长 %s)…", s, shutdownGrace)
			ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
			if err := httpServer.Shutdown(ctx); err != nil {
				log.Printf("[vproxy] 关闭超时/出错：%v(强制结束)", err)
				if closeErr := httpServer.Close(); closeErr != nil {
					log.Printf("[vproxy] 强制关闭监听连接失败：%v", closeErr)
				}
			}
			cancel()
			stopProxyHealthScheduler()
			stopProxySubscriptionScheduler()
			stopAdminSessionCleanup()
			stopProxyGC()
			flushCtx, flushCancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := nodes.FlushHealth(flushCtx); err != nil {
				log.Printf("[vproxy] 关闭前写入代理健康状态失败：%v", err)
			}
			flushCancel()
			transport.StopAllProxies()
			close(shutdownDone)
			return
		}
	}()

	poolStr := "关闭"
	if cfg.ParallelPoolEnabled() {
		poolStr = strconv.Itoa(cfg.ParallelPoolSize()) + "（开启）"
	}
	log.Printf("[vproxy] 监听 %s（API 密钥 %d 个，max_retries=%d，并发池=%s）",
		httpServer.Addr, keys.Count(), cfg.MaxRetries(), poolStr)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("[vproxy] server error: %v", err)
	}
	<-shutdownDone
	log.Printf("[vproxy] 关闭完成，程序退出")
}

func checkRulesAgreed(curHash string) bool {
	data, err := os.ReadFile(rulesAgreedPath())
	if err != nil {
		return false
	}
	return strings.Contains(string(data), curHash)
}

func hasOldAgreement() bool {
	data, err := os.ReadFile(rulesAgreedPath())
	if err != nil {
		return false
	}
	return len(strings.TrimSpace(string(data))) > 0
}

func saveRulesAgreed(curHash string) {
	path := rulesAgreedPath()
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	line := fmt.Sprintf("%s\t%s\n", time.Now().Format(time.RFC3339), curHash)
	_ = os.WriteFile(path, []byte(line), 0o600)
}

func checkRulesAgreedDocker(curHash string) bool {
	if strings.TrimSpace(os.Getenv("VPROXY_RULES_HASH")) == curHash {
		return true
	}
	data, err := os.ReadFile(rulesAgreedDockerPath())
	if err != nil {
		return false
	}
	return strings.Contains(string(data), curHash)
}
