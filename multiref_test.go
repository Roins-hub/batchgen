package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func twoRefs() GenInput {
	a := &RefImage{URL: "https://x/a.png", Mime: "image/png"}
	b := &RefImage{Data: []byte("bb"), Mime: "image/jpeg"}
	return GenInput{Model: "m", Prompt: "p", Ref: a, Refs: []*RefImage{a, b}}
}

func renderBody(t *testing.T, spec Spec, in GenInput) map[string]any {
	t.Helper()
	p, err := prepare(&spec, in, false)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(p.Body, &v); err != nil {
		t.Fatalf("%v: %s", err, p.Body)
	}
	return v
}

func TestSeedanceSingleVsManyRefs(t *testing.T) {
	spec, _ := presetByID("openai_video_seedance")
	many := renderBody(t, spec, twoRefs())["references"].(map[string]any)
	list, _ := many["images"].([]any)
	if len(list) != 2 || many["image"] != nil || list[0] != "https://x/a.png" || !strings.HasPrefix(list[1].(string), "data:image/jpeg;base64,") {
		t.Fatalf("多张图应该发 images 数组: %v", many)
	}
	in := twoRefs()
	in.Refs = nil
	one := renderBody(t, spec, in)["references"].(map[string]any)
	if one["image"] != "https://x/a.png" || one["images"] != nil {
		t.Fatalf("一张图应该发 image: %v", one)
	}
	in.Ref = nil
	if _, ok := renderBody(t, spec, in)["references"]; ok {
		t.Fatal("没有参考图时不应该带 references")
	}
}

func TestEachExpandsChatAndGemini(t *testing.T) {
	spec, _ := presetByID("openai_chat")
	msg := renderBody(t, spec, twoRefs())["messages"].([]any)[0].(map[string]any)
	if parts := msg["content"].([]any); len(parts) != 3 || parts[1].(map[string]any)["type"] != "image_url" {
		t.Fatalf("Chat 应该有文字加两张图: %v", parts)
	}
	in := twoRefs()
	in.Ref, in.Refs = nil, nil
	msg = renderBody(t, spec, in)["messages"].([]any)[0].(map[string]any)
	if parts := msg["content"].([]any); len(parts) != 1 {
		t.Fatalf("没有图时只有文字: %v", parts)
	}

	spec, _ = presetByID("gemini_image")
	in = twoRefs()
	in.Refs[0] = &RefImage{Data: []byte("aa"), Mime: "image/png"}
	in.Ref = in.Refs[0]
	content := renderBody(t, spec, in)["contents"].([]any)[0].(map[string]any)
	parts := content["parts"].([]any)
	if len(parts) != 3 || parts[2].(map[string]any)["inline_data"].(map[string]any)["mime_type"] != "image/jpeg" {
		t.Fatalf("Gemini 应该有文字加两张图: %v", parts)
	}
}

func TestOpenAIImagesFormFields(t *testing.T) {
	spec, _ := presetByID("openai_images")
	in := twoRefs()
	in.Refs[0].Data = []byte("aa")
	p, err := prepare(&spec, in, false)
	if err != nil {
		t.Fatal(err)
	}
	if c := bytes.Count(p.Body, []byte(`name="image[]"`)); c != 2 {
		t.Fatalf("多张图应该用 image[] 字段，实际 %d 个: %s", c, p.Preview)
	}
	in.Refs = nil
	p, err = prepare(&spec, in, false)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(p.Body, []byte(`name="image"`)) {
		t.Fatal("一张图保持 image 字段，兼容只认这个字段的接口")
	}
}

func TestAcceptsManyRefs(t *testing.T) {
	for id, want := range map[string]bool{"openai_images": true, "openai_chat": true, "gemini_image": true, "seedream": true,
		"openai_video_seedance": true, "kling_video": false, "sora": false, "newapi_video": true} {
		spec, ok := presetByID(id)
		if !ok {
			t.Fatalf("没有预设 %s", id)
		}
		if got := spec.acceptsManyRefs(); got != want {
			t.Errorf("%s acceptsManyRefs = %v，应为 %v", id, got, want)
		}
	}
}

func TestRefreshPresetKeepsUserSettings(t *testing.T) {
	s, _ := presetByID("openai_video_seedance")
	s.Submit.Body = `{"model":"{{model}}"}`
	s.BaseURL, s.ImageDelivery, s.APIKeys = "https://my", "inline", []string{"k"}
	s.refreshPreset()
	if !strings.Contains(s.Submit.Body, "references_images") {
		t.Fatal("内置格式应该更新到新模板")
	}
	if s.BaseURL != "https://my" || s.ImageDelivery != "inline" || len(s.APIKeys) != 1 {
		t.Fatalf("用户设置被覆盖: %+v", s)
	}
	c := s
	c.Preset, c.Submit.Body = "custom", `{"mine":true}`
	c.refreshPreset()
	if c.Submit.Body != `{"mine":true}` {
		t.Fatal("自定义格式不能被覆盖")
	}
}

func TestCreateBatchRefLimits(t *testing.T) {
	s := pipelineServer(t)
	png := []byte("\x89PNG\r\n\x1a\n0000")
	send := func(n int, kind string) *httptest.ResponseRecorder {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		_ = mw.WriteField("kind", kind)
		_ = mw.WriteField("model", "m")
		_ = mw.WriteField("prompts", "一只柴犬")
		for i := range n {
			p, _ := mw.CreateFormFile("ref", fmt.Sprintf("%d.png", i))
			p.Write(png)
		}
		mw.Close()
		req := httptest.NewRequest(http.MethodPost, "/api/batches", &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		return post(s, req)
	}

	if w := send(4, "image"); w.Code != http.StatusOK {
		t.Fatalf("默认应该能传 4 张: %d %s", w.Code, w.Body)
	}
	j := s.store.batches[0].Jobs[0]
	if j.Image == "" || len(j.Refs) != 3 || !strings.HasSuffix(j.Refs[2], "_4.png") {
		t.Fatalf("参考图没有全部保存: %+v", j)
	}
	if w := send(5, "image"); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "最多上传 4 张") {
		t.Fatalf("超过上限应该拒绝: %d %s", w.Code, w.Body)
	}
	c := s.cfg.Get()
	c.MaxRefImages = 5
	if err := s.cfg.Set(c); err != nil {
		t.Fatal(err)
	}
	if w := send(5, "image"); w.Code != http.StatusOK {
		t.Fatalf("调大上限后应该能传 5 张: %d %s", w.Code, w.Body)
	}
	// 只支持一张图的格式要在创建时说清楚，而不是悄悄只发第一张。
	c = s.cfg.Get()
	c.VideoSpec, _ = presetByID("kling_video")
	if err := s.cfg.Set(c); err != nil {
		t.Fatal(err)
	}
	if w := send(2, "video"); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "只能用 1 张") {
		t.Fatalf("不支持多图的接口应该拒绝: %d %s", w.Code, w.Body)
	}
}

func TestCSVMultipleImages(t *testing.T) {
	rows, err := parseCSV([]byte("prompt,image\nx,https://a/1.png | https://a/2.png|https://a/3.png\n"))
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Image != "https://a/1.png" || len(rows[0].Refs) != 2 || rows[0].Refs[1] != "https://a/3.png" {
		t.Fatalf("多张图解析不对: %+v", rows[0])
	}
}

// 回归：线路慢时响应要传很久，只要数据还在流动就不能因为「总时长」超时而丢掉结果。
func TestSlowResponseKeepsWaitingWhileDataFlows(t *testing.T) {
	old := stallTimeout
	stallTimeout = 150 * time.Millisecond
	defer func() { stallTimeout = old }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for range 8 { // 总共 0.8 秒，比 wait 和 stallTimeout 都长
			fmt.Fprint(w, "          ")
			w.(http.Flusher).Flush()
			time.Sleep(100 * time.Millisecond)
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()
	var seen int64
	data, err := NewClient(srv.URL, "k", "bearer", nil).callWait(context.Background(), http.MethodGet, "/", nil, "", 200*time.Millisecond,
		func(got, _ int64) { seen = got })
	if err != nil {
		t.Fatalf("数据还在流动时不该超时: %v", err)
	}
	if !strings.Contains(string(data), `"ok"`) || seen == 0 {
		t.Fatalf("结果或进度不对: %q %d", data, seen)
	}
}

func TestStalledResponseFailsWithClearReason(t *testing.T) {
	old := stallTimeout
	stallTimeout = 100 * time.Millisecond
	defer func() { stallTimeout = old }()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "partial")
		w.(http.Flusher).Flush()
		<-release
	}))
	defer srv.Close()
	defer close(release)
	_, err := NewClient(srv.URL, "k", "bearer", nil).callWait(context.Background(), http.MethodGet, "/", nil, "", time.Second, nil)
	if err == nil || !strings.Contains(err.Error(), "没有新数据") {
		t.Fatalf("应该说明是传输中断: %v", err)
	}
}

func TestNoResponseFailsWithClearReason(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	_, err := NewClient(srv.URL, "k", "bearer", nil).callWait(context.Background(), http.MethodGet, "/", nil, "", 100*time.Millisecond, nil)
	if err == nil || !strings.Contains(err.Error(), "接口没有响应") {
		t.Fatalf("应该说明接口没响应: %v", err)
	}
	if errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "context") {
		t.Fatalf("不该把内部错误露给用户: %v", err)
	}
}

func TestUsesHTTP1(t *testing.T) {
	// HTTP/2 让同时生成的任务共用一条连接，一断全丢。
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, r.Proto) }))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	c := newHTTPClient()
	tr := c.Transport.(*http.Transport)
	tr.TLSClientConfig.InsecureSkipVerify = true
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Proto != "HTTP/1.1" {
		t.Fatalf("应该走 HTTP/1.1，实际 %s", resp.Proto)
	}
}

func TestNotSentIsRetriedWithoutCharge(t *testing.T) {
	// 握手阶段就断开：请求没发出去。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	_, err = NewClient("https://"+ln.Addr().String(), "k", "bearer", nil).callWait(context.Background(), http.MethodPost, "/", []byte("x"), "", time.Second, nil)
	var ns *notSentError
	if !errors.As(err, &ns) {
		t.Fatalf("没发出去的请求应该标记出来: %v", err)
	}

	oldN, oldD := connectRetries, connectDelay
	defer func() { connectRetries, connectDelay = oldN, oldD }()
	connectRetries, connectDelay = 3, time.Millisecond
	calls := 0
	_, err = withRetry(context.Background(), 0, func() (int, error) {
		calls++
		if calls < 3 {
			return 0, ns
		}
		return 1, nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("应该重试到成功: %v %d", err, calls)
	}
	calls = 0
	_, _ = withRetry(context.Background(), 0, func() (int, error) { calls++; return 0, ns })
	if calls != 4 {
		t.Fatalf("重试次数不对: %d", calls)
	}
}

func TestResetAfterSendIsNotRetried(t *testing.T) {
	// 回归：请求发出后连接被重置，接口可能已经扣费，不能自动重试。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		c, _, _ := w.(http.Hijacker).Hijack()
		c.(*net.TCPConn).SetLinger(0)
		c.Close()
	}))
	defer srv.Close()
	calls := 0
	_, err := withRetry(context.Background(), 3, func() ([]byte, error) {
		calls++
		return NewClient(srv.URL, "k", "bearer", nil).callWait(context.Background(), http.MethodPost, "/", []byte("x"), "", time.Second, nil)
	})
	if calls != 1 {
		t.Fatalf("不能自动重试，实际请求了 %d 次", calls)
	}
	if err == nil || !strings.Contains(err.Error(), "可能已生成并扣费") {
		t.Fatalf("要告诉用户可能已扣费: %v", err)
	}
}

func TestManualProxyWins(t *testing.T) {
	defer setManualProxy("")
	setManualProxy("http://127.0.0.1:7890")
	u, err := httpClient.Transport.(*http.Transport).Proxy(httptest.NewRequest(http.MethodGet, "https://api.example.com/", nil))
	if err != nil || u == nil || u.Host != "127.0.0.1:7890" {
		t.Fatalf("应该用设置里的代理: %v %v", u, err)
	}
	c := defaultConfig()
	c.ProxyURL = "ftp://x"
	c.normalize()
	if err := c.validate(); err == nil {
		t.Fatal("不支持的代理协议要报错")
	}
	c.ProxyURL = "127.0.0.1:7890"
	c.normalize()
	if err := c.validate(); err != nil || c.ProxyURL != "http://127.0.0.1:7890" {
		t.Fatalf("没写协议时按 http: %v %s", err, c.ProxyURL)
	}
}

func TestImageRatioBecomesPixels(t *testing.T) {
	s := pipelineServer(t)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, v := range map[string]string{"kind": "image", "model": "m", "prompts": "一只柴犬", "size": "21:9"} {
		_ = mw.WriteField(k, v)
	}
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/batches", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if w := post(s, req); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if got := s.store.batches[0].Jobs[0].Size; got != "1680x720" {
		t.Fatalf("比例要换算成像素: %s", got)
	}
}

func TestNewAPIVideoSendsAllImages(t *testing.T) {
	spec, _ := presetByID("newapi_video")
	v := renderBody(t, spec, twoRefs())
	if list, _ := v["images"].([]any); len(list) != 2 || v["image"] != "https://x/a.png" {
		t.Fatalf("多张图要放进 images，image 保留第一张兼容旧渠道: %v", v)
	}
}

func TestWrongVideoFormatTellsWhichToUse(t *testing.T) {
	err := &APIError{Status: 400, Body: `{"error":{"message":"invalid api platform: 60"}}`}
	if !strings.Contains(err.Error(), "Seedance 扩展") {
		t.Fatalf("要告诉用户换哪个格式: %v", err)
	}
}

func TestNoChannelOnVideoGenerationsTellsWhichFormat(t *testing.T) {
	const route = "/v1/video/generations"
	for _, body := range []string{
		`{"error":{"message":"No available channel for model seedance-2.0-fast under group default (distributor) (request id: 1)"}}`,
		`{"error":{"message":"分组 default 下模型 seedance-2.0-fast 无可用渠道（distributor）"}}`,
	} {
		got := (&APIError{Status: 503, Body: body, Path: route}).Error()
		hint, origin := strings.Index(got, "Seedance 扩展"), strings.Index(got, "seedance-2.0-fast")
		if hint < 0 || origin < 0 || hint > origin {
			t.Fatalf("卡片只显示开头，提示要在原始报错前面: %q", got)
		}
	}
	// 其它路径上的无可用渠道是真的没渠道，不能让用户去换格式
	other := &APIError{Status: 503, Body: `{"error":{"message":"No available channel for model x"}}`, Path: "/v1/videos"}
	if strings.Contains(other.Error(), "Seedance 扩展") {
		t.Fatalf("/v1/videos 不该提示换格式: %v", other)
	}
}

func TestDefaultVideoFormatUsesVideosRoute(t *testing.T) {
	c := defaultConfig()
	c.normalize()
	if c.VideoSpec.Submit.Path != "/v1/videos" || c.VideoSpec.ImageDelivery != "upload" {
		t.Fatalf("新用户默认要用 /v1/videos，并把本地图传到图床: %+v", c.VideoSpec.Submit.Path)
	}
	// 已经保存过的格式不能被默认值改掉
	saved := defaultConfig()
	saved.VideoSpec, _ = presetByID("newapi_video")
	saved.normalize()
	if saved.VideoSpec.Submit.Path != "/v1/video/generations" {
		t.Fatalf("不能改用户保存的格式: %s", saved.VideoSpec.Submit.Path)
	}
}
