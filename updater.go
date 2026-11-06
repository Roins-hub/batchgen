package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 下面三个值在编译时由 build.sh 用 -ldflags -X 写入，dev 版本不检查更新。
var (
	releaseVersion = "dev"
	updateURL      = "https://github.com/Roins-hub/batchgen/releases/latest/download/latest.json"
	// updatePublicKey 验证更新包的签名。私钥只在发布者电脑上，存储被人动了手脚也装不上。
	updatePublicKey = "JwyVjpcOwDgHgONenPyvBBXFzVM1VDezS9J9rlGIRls="
)

// updateMirrors 是内置的 GitHub 下载镜像，直连失败时依次尝试：把 GitHub 地址拼在镜像地址后面。
// 镜像是第三方的，但更新信息有签名、程序有校验值，镜像改不了内容。
var updateMirrors = []string{"https://gh-proxy.com/", "https://ghfast.top/"}

// checkSettle：第一个有效结果到达后，再等其它来源多久。
var checkSettle = 1500 * time.Millisecond

const (
	updateInterval = 6 * time.Hour
	maxUpdateBytes = 200 << 20
	sourceTimeout  = 15 * time.Second // 检查更新最长等待时间
	firstByteWait  = 30 * time.Second
)

// Release 是 latest.json 里被签名的内容。
type Release struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
	Notes   string `json:"notes,omitempty"`
}

// verifyManifest 校验 latest.json 的签名并解析。manifest 是字符串，保证验签的就是发布时签的那些字节。
func verifyManifest(raw []byte, pub ed25519.PublicKey) (Release, error) {
	var signed struct {
		Manifest  string `json:"manifest"`
		Signature string `json:"signature"`
	}
	var r Release
	if err := json.Unmarshal(raw, &signed); err != nil {
		return r, errors.New("更新信息格式错误")
	}
	sig, err := base64.StdEncoding.DecodeString(signed.Signature)
	if err != nil || len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, []byte(signed.Manifest), sig) {
		return r, errors.New("更新信息的签名不正确，已忽略")
	}
	if err := json.Unmarshal([]byte(signed.Manifest), &r); err != nil {
		return r, errors.New("更新信息格式错误")
	}
	return r, nil
}

// newerThan 比较 1.2.10 这样的版本号：a 比 b 新时返回 true。
func newerThan(a, b string) bool {
	x, y := strings.Split(a, "."), strings.Split(b, ".")
	for i := range max(len(x), len(y)) {
		var m, n int
		if i < len(x) {
			m, _ = strconv.Atoi(x[i])
		}
		if i < len(y) {
			n, _ = strconv.Atoi(y[i])
		}
		if m != n {
			return m > n
		}
	}
	return false
}

// Updater 检查和安装新版本。
type Updater struct {
	idle   func() bool   // 没有任务在跑或排队时返回 true
	mirror func() string // 用户在设置里填的镜像地址，优先尝试

	mu    sync.Mutex
	rel   *Release
	via   string // 上次成功取到更新信息的镜像，下载时优先用它
	state string // idle | downloading | waiting | installing | failed
	err   string
	got   atomic.Int64
}

func NewUpdater(idle func() bool) *Updater { return &Updater{idle: idle, state: "idle"} }

func updatePaths() (exe, next, old string, err error) {
	if exe, err = os.Executable(); err != nil {
		return
	}
	base := strings.TrimSuffix(exe, ".exe")
	return exe, base + ".update", base + ".old.exe", nil
}

// Loop 启动时清理上次更新留下的文件，然后定期检查新版本。
func (u *Updater) Loop() {
	if releaseVersion == "dev" {
		return
	}
	go removeLeftovers()
	time.Sleep(3 * time.Second)
	for {
		_ = u.check(context.Background()) // 检查失败不影响使用，下次再试
		time.Sleep(updateInterval)
	}
}

// removeLeftovers 删除旧版本文件。旧进程可能还没完全退出，多试几次。
func removeLeftovers() {
	_, next, old, err := updatePaths()
	if err != nil {
		return
	}
	os.Remove(next)
	for range 20 {
		if err := os.Remove(old); err == nil || errors.Is(err, os.ErrNotExist) {
			return
		}
		time.Sleep(time.Second)
	}
}

// sources 返回依次尝试的前缀：用户填的镜像、preferred、直连、内置镜像，去掉重复。空字符串表示直连。
func (u *Updater) sources(preferred string) []string {
	var out []string
	add := func(src string) {
		if !slices.Contains(out, src) {
			out = append(out, src)
		}
	}
	if u.mirror != nil && u.mirror() != "" {
		add(u.mirror())
	}
	if preferred != "" {
		add(preferred)
	}
	add("") // 直连
	for _, m := range updateMirrors {
		add(m)
	}
	return out
}

func (u *Updater) fetchManifest(ctx context.Context, src string) (Release, error) {
	var r Release
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src+updateURL, nil)
	if err != nil {
		return r, err
	}
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := httpClient.Do(req)
	if err != nil {
		return r, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return r, fmt.Errorf("更新服务器返回 HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return r, err
	}
	pub, err := base64.StdEncoding.DecodeString(updatePublicKey)
	if err != nil {
		return r, err
	}
	return verifyManifest(raw, pub)
}

// check 同时询问所有来源，第一个有效结果到达后再等片刻，取其中最高的版本：
// 挨个试时一个来源不通就要等到超时，用户点了「检查更新」要等很久；镜像可能缓存着旧版本，不能只看最快的。
func (u *Updater) check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, sourceTimeout)
	defer cancel()
	type result struct {
		src string
		rel Release
		err error
	}
	srcs := u.sources("")
	results := make(chan result, len(srcs))
	for _, src := range srcs {
		go func() {
			r, err := u.fetchManifest(ctx, src)
			results <- result{src, r, err}
		}()
	}
	var best *result
	var settled <-chan time.Time
wait:
	for range srcs {
		select {
		case res := <-results:
			if res.err == nil && (best == nil || newerThan(res.rel.Version, best.rel.Version)) {
				best = &res
			}
			if best != nil && settled == nil {
				settled = time.After(checkSettle)
			}
		case <-settled:
			break wait
		case <-ctx.Done():
			break wait
		}
	}
	if best == nil {
		return errors.New("连不上更新服务器，GitHub 和镜像都没有响应。请检查网络，或在「设置」里填写更新镜像地址")
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.via = best.src
	if newerThan(best.rel.Version, releaseVersion) {
		u.rel = &best.rel
	}
	return nil
}

// Apply 开始更新：下载、校验，等任务都结束后替换程序并重启。
func (u *Updater) Apply() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.rel == nil {
		return errors.New("没有可用的新版本")
	}
	if u.state != "idle" && u.state != "failed" {
		return nil
	}
	u.state, u.err = "downloading", ""
	u.got.Store(0)
	go u.run(*u.rel)
	return nil
}

func (u *Updater) setState(s string) {
	u.mu.Lock()
	u.state = s
	u.mu.Unlock()
}

func (u *Updater) fail(err error) {
	u.mu.Lock()
	u.state, u.err = "failed", err.Error()
	u.mu.Unlock()
}

func (u *Updater) run(r Release) {
	exe, next, old, err := updatePaths()
	if err == nil {
		err = u.downloadAny(r, next)
	}
	if err != nil {
		u.fail(err)
		return
	}
	u.setState("waiting")
	for !u.idle() {
		time.Sleep(2 * time.Second)
	}
	u.setState("installing")
	if err := install(exe, next, old); err != nil {
		u.fail(err)
	}
}

// downloadAny 依次试各个来源，从取到更新信息的那个开始。
func (u *Updater) downloadAny(r Release, dest string) error {
	u.mu.Lock()
	via := u.via
	u.mu.Unlock()
	var err error
	for _, src := range u.sources(via) {
		u.got.Store(0)
		if err = u.download(r, src+r.URL, dest); err == nil {
			return nil
		}
	}
	return err
}

func (u *Updater) download(r Release, url, dest string) error {
	if r.Size <= 0 || r.Size > maxUpdateBytes {
		return errors.New("更新包大小不正确")
	}
	ctx, touch, stop := stallGuard(context.Background(), firstByteWait, func() error {
		return fmt.Errorf("下载时 %d 秒没有新数据，请检查网络后重试", int(stallTimeout.Seconds()))
	})
	defer stop()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return friendlyCause(ctx, err)
	}
	defer resp.Body.Close()
	touch(stallTimeout)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("下载更新包失败：HTTP %d", resp.StatusCode)
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return fmt.Errorf("程序所在文件夹不能写入，无法自动更新。请把程序放到普通文件夹（不要放在 Program Files）：%w", err)
	}
	var h hash.Hash = sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), io.LimitReader(&countReader{resp.Body, func(n int) {
		touch(stallTimeout)
		u.got.Add(int64(n))
	}}, maxUpdateBytes))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dest)
		return friendlyCause(ctx, err)
	}
	if hex.EncodeToString(h.Sum(nil)) != r.SHA256 {
		os.Remove(dest)
		return errors.New("更新包校验失败，已丢弃。请稍后重试")
	}
	return nil
}

// install 替换程序并启动新版本。Windows 不能覆盖运行中的 exe，但可以改名：
// 把自己挪成 .old.exe，新文件放到原位置。新版本启动后走现有的版本接管流程，让本进程退出。
func install(exe, next, old string) error {
	os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		return fmt.Errorf("替换程序失败：%w", err)
	}
	restore := func() { os.Rename(exe, next); os.Rename(old, exe) }
	if err := os.Rename(next, exe); err != nil {
		os.Rename(old, exe)
		return fmt.Errorf("替换程序失败：%w", err)
	}
	cmd := exec.Command(exe)
	if err := cmd.Start(); err != nil {
		restore()
		return fmt.Errorf("启动新版本失败：%w", err)
	}
	// 正常情况下新版本很快就会让本进程退出。还活着说明新版本没起来，换回旧版本继续用。
	time.Sleep(30 * time.Second)
	cmd.Process.Kill()
	time.Sleep(time.Second)
	restore()
	return errors.New("新版本没有正常启动，已恢复原来的版本")
}

func (s *Server) updateStatus(w http.ResponseWriter, r *http.Request) {
	u := s.updater
	u.mu.Lock()
	defer u.mu.Unlock()
	out := map[string]any{"current": releaseVersion, "state": u.state, "error": u.err, "got": u.got.Load()}
	if u.rel != nil {
		out["version"], out["notes"], out["size"] = u.rel.Version, u.rel.Notes, u.rel.Size
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) updateApply(w http.ResponseWriter, r *http.Request) {
	if err := s.updater.Apply(); err != nil {
		httpErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// updateCheck 是用户手动点「检查更新」。
func (s *Server) updateCheck(w http.ResponseWriter, r *http.Request) {
	if releaseVersion == "dev" {
		httpErr(w, http.StatusBadRequest, "开发版本不检查更新")
		return
	}
	if err := s.updater.check(r.Context()); err != nil {
		httpErr(w, http.StatusBadGateway, "检查更新失败："+err.Error())
		return
	}
	s.updateStatus(w, r)
}
