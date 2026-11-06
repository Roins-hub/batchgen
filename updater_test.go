package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func signedManifest(t *testing.T, priv ed25519.PrivateKey, r Release) []byte {
	t.Helper()
	m, _ := json.Marshal(r)
	out, _ := json.Marshal(map[string]string{"manifest": string(m), "signature": base64.StdEncoding.EncodeToString(ed25519.Sign(priv, m))})
	return out
}

func TestVerifyManifest(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	r := Release{Version: "1.3.0", URL: "https://x/a.exe", SHA256: "ab", Size: 5}
	raw := signedManifest(t, priv, r)
	got, err := verifyManifest(raw, pub)
	if err != nil || got != r {
		t.Fatalf("合法的更新信息应该通过: %v %+v", err, got)
	}
	tampered := strings.Replace(string(raw), "https://x/a.exe", "https://evil/a.exe", 1)
	if _, err := verifyManifest([]byte(tampered), pub); err == nil {
		t.Fatal("改过的更新信息必须被拒绝")
	}
	other, _, _ := ed25519.GenerateKey(nil)
	if _, err := verifyManifest(raw, other); err == nil {
		t.Fatal("别的密钥签的必须被拒绝")
	}
	if _, err := verifyManifest([]byte(`{"manifest":"{}"}`), pub); err == nil {
		t.Fatal("没有签名必须被拒绝")
	}
}

func TestPublicKeyIsValid(t *testing.T) {
	if k, err := base64.StdEncoding.DecodeString(updatePublicKey); err != nil || len(k) != ed25519.PublicKeySize {
		t.Fatalf("内置公钥无效: %v", err)
	}
}

func TestNewerThan(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{{"1.3.0", "1.2.0", true}, {"1.2.10", "1.2.9", true}, {"1.2.0", "1.2.0", false}, {"1.2.0", "1.3.0", false}, {"2.0", "1.9.9", true}, {"1.2.0.1", "1.2", true}} {
		if got := newerThan(c.a, c.b); got != c.want {
			t.Errorf("newerThan(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

func TestCheckOnlyOffersNewerSignedVersion(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	body := []byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	defer srv.Close()
	oldURL, oldKey, oldVer := updateURL, updatePublicKey, releaseVersion
	defer func() { updateURL, updatePublicKey, releaseVersion = oldURL, oldKey, oldVer }()
	updateURL, updatePublicKey, releaseVersion = srv.URL, base64.StdEncoding.EncodeToString(pub), "1.3.0"
	oldM := updateMirrors
	defer func() { updateMirrors = oldM }()
	updateMirrors = nil

	u := NewUpdater(func() bool { return true })
	body = signedManifest(t, priv, Release{Version: "1.3.0"})
	if err := u.check(t.Context()); err != nil || u.rel != nil {
		t.Fatalf("相同版本不应该提示更新: %v %+v", err, u.rel)
	}
	body = signedManifest(t, priv, Release{Version: "1.2.0"})
	if err := u.check(t.Context()); err != nil || u.rel != nil {
		t.Fatalf("不能降级: %v %+v", err, u.rel)
	}
	body = signedManifest(t, priv, Release{Version: "1.4.0", Size: 9})
	if err := u.check(t.Context()); err != nil || u.rel == nil || u.rel.Version != "1.4.0" {
		t.Fatalf("应该提示新版本: %v %+v", err, u.rel)
	}
}

func TestDownloadRejectsWrongHash(t *testing.T) {
	data := []byte("MZ new program")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(data) }))
	defer srv.Close()
	sum := sha256.Sum256(data)
	dest := filepath.Join(t.TempDir(), "next")
	u := NewUpdater(func() bool { return true })

	r := Release{Size: int64(len(data)), SHA256: strings.Repeat("0", 64)}
	if err := u.download(r, srv.URL, dest); err == nil || !strings.Contains(err.Error(), "校验失败") {
		t.Fatalf("校验不过必须失败: %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("校验失败的文件要删除")
	}
	r.SHA256 = hex.EncodeToString(sum[:])
	if err := u.download(r, srv.URL, dest); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dest); string(got) != string(data) || u.got.Load() == 0 {
		t.Fatalf("下载内容或进度不对: %q %d", got, u.got.Load())
	}
}

func TestSourcesOrder(t *testing.T) {
	old := updateMirrors
	defer func() { updateMirrors = old }()
	updateMirrors = []string{"https://m1/", "https://m2/"}
	u := NewUpdater(func() bool { return true })
	if got := u.sources(""); !slices.Equal(got, []string{"", "https://m1/", "https://m2/"}) {
		t.Fatalf("默认先直连再镜像: %q", got)
	}
	if got := u.sources("https://m2/"); !slices.Equal(got, []string{"https://m2/", "", "https://m1/"}) {
		t.Fatalf("下载时先用成功过的来源: %q", got)
	}
	u.mirror = func() string { return "https://mine/" }
	if got := u.sources(""); !slices.Equal(got, []string{"https://mine/", "", "https://m1/", "https://m2/"}) {
		t.Fatalf("用户填的镜像最先: %q", got)
	}
}

// 回归：直连失败时要能换镜像，错误的镜像内容不能被接受。
func TestFailoverToMirror(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	data := []byte("MZ new program")
	sum := sha256.Sum256(data)
	var manifest []byte
	// 镜像地址是 http://host/镜像名/ 加上 GitHub 地址。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/dead/"):
			w.WriteHeader(http.StatusBadGateway)
		case strings.HasPrefix(r.URL.Path, "/evil/") && strings.HasSuffix(r.URL.Path, "latest.json"):
			w.Write(signedManifest(t, mustKey(t), Release{Version: "9.9.9"}))
		case strings.HasPrefix(r.URL.Path, "/evil/"):
			w.Write([]byte("MZ malware!!!!"))
		case strings.HasSuffix(r.URL.Path, "latest.json"):
			w.Write(manifest)
		default:
			w.Write(data)
		}
	}))
	defer srv.Close()
	oldURL, oldKey, oldVer, oldM := updateURL, updatePublicKey, releaseVersion, updateMirrors
	defer func() { updateURL, updatePublicKey, releaseVersion, updateMirrors = oldURL, oldKey, oldVer, oldM }()
	updateURL, updatePublicKey, releaseVersion = "/latest.json", base64.StdEncoding.EncodeToString(pub), "1.0.0"
	updateMirrors = []string{srv.URL + "/evil", srv.URL + "/ok"}
	manifest = signedManifest(t, priv, Release{Version: "2.0.0", URL: "/app.exe", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))})

	u := NewUpdater(func() bool { return true })
	u.mirror = func() string { return srv.URL + "/dead" }
	if err := u.check(t.Context()); err != nil {
		t.Fatal(err)
	}
	if u.rel == nil || u.rel.Version != "2.0.0" || u.via != srv.URL+"/ok" {
		t.Fatalf("应该跳过失效和伪造的镜像: %+v via=%q", u.rel, u.via)
	}
	dest := filepath.Join(t.TempDir(), "next")
	if err := u.downloadAny(*u.rel, dest); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dest); string(got) != string(data) {
		t.Fatalf("下载内容不对: %q", got)
	}
	// 只有伪造内容的镜像时，不能装上。
	u.via, updateMirrors = "", []string{srv.URL + "/evil"}
	u.mirror = nil
	if err := u.downloadAny(*u.rel, dest); err == nil {
		t.Fatal("校验不过的文件不能算下载成功")
	}
}

// 回归：一个来源卡住时，检查更新不能跟着等到超时；镜像的旧缓存不能盖过新版本。
func TestCheckDoesNotWaitForSlowSource(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/hang/"):
			<-release
		case strings.HasPrefix(r.URL.Path, "/stale/"):
			w.Write(signedManifest(t, priv, Release{Version: "1.5.0"}))
		default:
			time.Sleep(100 * time.Millisecond)
			w.Write(signedManifest(t, priv, Release{Version: "2.0.0"}))
		}
	}))
	defer srv.Close()
	defer close(release)
	oldURL, oldKey, oldVer, oldM, oldS := updateURL, updatePublicKey, releaseVersion, updateMirrors, checkSettle
	defer func() {
		updateURL, updatePublicKey, releaseVersion, updateMirrors, checkSettle = oldURL, oldKey, oldVer, oldM, oldS
	}()
	updateURL, updatePublicKey, releaseVersion = "/latest.json", base64.StdEncoding.EncodeToString(pub), "1.0.0"
	updateMirrors, checkSettle = []string{srv.URL + "/hang", srv.URL + "/stale", srv.URL + "/fresh"}, 400*time.Millisecond

	u := NewUpdater(func() bool { return true })
	start := time.Now()
	if err := u.check(t.Context()); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("不应该等卡住的来源: %v", took)
	}
	if u.rel == nil || u.rel.Version != "2.0.0" || u.via != srv.URL+"/fresh" {
		t.Fatalf("要取最高版本: %+v via=%q", u.rel, u.via)
	}
}

func TestCheckFailureNamesAllSources(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	oldURL, oldM := updateURL, updateMirrors
	defer func() { updateURL, updateMirrors = oldURL, oldM }()
	updateURL, updateMirrors = srv.URL, nil
	err := NewUpdater(func() bool { return true }).check(t.Context())
	if err == nil || !strings.Contains(err.Error(), "连不上更新服务器") || strings.Contains(err.Error(), "http") {
		t.Fatalf("要给一句看得懂的话，不要原始地址: %v", err)
	}
}

func mustKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, k, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestIdleSeesQueuedJobs(t *testing.T) {
	s := pipelineServer(t)
	if !s.runner.Idle() {
		t.Fatal("没有任务时应该空闲")
	}
	b := createPipeline(t, s, nil)
	if s.runner.Idle() {
		t.Fatal("有排队任务时不能更新")
	}
	for _, j := range b.Jobs {
		j.Status = StatusSuccess
	}
	if !s.runner.Idle() {
		t.Fatal("任务都完成后应该空闲")
	}
}
