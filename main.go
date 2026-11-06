package main

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

//go:embed web
var webFS embed.FS

const (
	appName  = "BatchGen 批量生成器"
	basePort = 17860
)

var appVersion = "dev"

func main() {
	appVersion = buildVersion()
	dir, err := appDir()
	if err != nil {
		fatal("无法确定数据目录", err)
	}

	ln, err := acquireListener()
	if err != nil {
		fatal("无法监听本地端口", err)
	}
	if ln == nil {
		return // 同版本已在运行，已切到它的窗口
	}

	// 拿到端口后再读数据：旧版本退出前写入的最终状态也能读到。
	cfg, err := LoadConfig(filepath.Join(dir, "config.json"), dir)
	if err != nil {
		fatal("读取配置失败", err)
	}
	store, err := LoadStore(filepath.Join(dir, "jobs.json"))
	if err != nil {
		fatal("读取任务记录失败", err)
	}
	serve(ln, store, cfg, dir)
}

// acquireListener 占用本地端口。端口被本程序占着时：同一版本 → 切到它的窗口并返回 nil；
// 不同版本 → 请旧版本退出后接管（旧版本有任务在跑时会拒绝，这时切到旧版本并说明原因）。
func acquireListener() (net.Listener, error) {
	// BATCHGEN_PORT 用于开发测试：换一个端口运行，不和正在使用的程序冲突。
	port := basePort
	if p, err := strconv.Atoi(os.Getenv("BATCHGEN_PORT")); err == nil && p > 0 {
		port = p
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		return ln, nil
	}
	existing := "http://" + addr
	running, ver := pingApp(existing)
	if !running {
		return listenFallback(port)
	}
	if ver == appVersion {
		openExisting(existing)
		return nil, nil
	}
	if err := askShutdown(existing); err != nil {
		openExisting(existing)
		showError(fmt.Sprintf("无法替换旧版本：%v", err))
		return nil, nil
	}
	for range 50 {
		if ln, err = net.Listen("tcp", addr); err == nil {
			return ln, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, err
}

func openExisting(url string) {
	if os.Getenv("BATCHGEN_NO_BROWSER") == "" && !focusRunning() {
		openBrowser(url)
	}
}

// buildVersion 用 exe 的修改时间和大小区分不同的编译版本。
func buildVersion() string {
	exe, err := os.Executable()
	if err != nil {
		return "dev"
	}
	fi, err := os.Stat(exe)
	if err != nil {
		return "dev"
	}
	return fi.ModTime().Format("2006-01-02 15:04:05") + " #" + strconv.FormatInt(fi.Size()%46656, 36)
}

func serve(ln net.Listener, store *Store, cfg *ConfigStore, dir string) {
	runner := NewRunner(store, cfg)
	runner.Recover()
	go runner.Loop()

	static, _ := fs.Sub(webFS, "web")
	updater := NewUpdater(runner.Idle)
	updater.mirror = func() string { return cfg.Get().UpdateMirror }
	go updater.Loop()
	srv := &Server{store: store, cfg: cfg, runner: runner, updater: updater, dataDir: dir, static: static}

	go func() { fatal("服务异常退出", http.Serve(ln, srv.Routes())) }()

	if os.Getenv("BATCHGEN_NO_BROWSER") != "" {
		select {}
	}
	runUI("http://"+ln.Addr().String(), dir, runner.ActiveCount)
}

func fatal(msg string, err error) {
	showError(fmt.Sprintf("%s：%v", msg, err))
	os.Exit(1)
}

// appDir 返回数据目录：默认是 exe 所在目录，go run 时退回当前目录，可用 BATCHGEN_DATA 覆盖。
func appDir() (string, error) {
	if d := os.Getenv("BATCHGEN_DATA"); d != "" {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", err
		}
		return filepath.Abs(d)
	}
	exe, err := os.Executable()
	if err != nil {
		return os.Getwd()
	}
	dir := filepath.Dir(exe)
	if strings.Contains(strings.ToLower(dir), "go-build") {
		return os.Getwd()
	}
	return dir, nil
}

func listenFallback(base int) (net.Listener, error) {
	var lastErr error
	for port := base + 1; port < base+20; port++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			return ln, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func pingApp(base string) (bool, string) {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(base + "/api/ping")
	if err != nil {
		return false, ""
	}
	defer resp.Body.Close()
	var v struct{ App, Version string }
	if json.NewDecoder(io.LimitReader(resp.Body, 1024)).Decode(&v) != nil || v.App != "batchgen" {
		return false, ""
	}
	return true, v.Version
}

func askShutdown(base string) error {
	req, _ := http.NewRequest(http.MethodPost, base+"/api/shutdown", nil)
	req.Header.Set("X-BatchGen", "1")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	var v struct{ Error string }
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&v)
	if v.Error == "" {
		v.Error = "旧版本不支持自动替换，请手动关闭旧版本的窗口后再打开"
	}
	return errors.New(v.Error)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

// revealFile 打开文件所在文件夹并选中它。
func revealFile(path string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("explorer", "/select,", path).Start()
	case "darwin":
		return exec.Command("open", "-R", path).Start()
	default:
		return openFolder(filepath.Dir(path))
	}
}

func openFolder(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer", path)
	case "darwin":
		cmd = exec.Command("open", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	return cmd.Start()
}
