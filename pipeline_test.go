package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func pipelineServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	cfg, err := LoadConfig(filepath.Join(dir, "config.json"), dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := LoadStore(filepath.Join(dir, "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	return &Server{store: store, cfg: cfg, runner: NewRunner(store, cfg), dataDir: dir}
}

func post(s *Server, req *http.Request) *httptest.ResponseRecorder {
	req.Header.Set("X-BatchGen", "1")
	req.Host = "127.0.0.1"
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, req)
	return w
}

func createPipeline(t *testing.T, s *Server, fields map[string]string) *Batch {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	base := map[string]string{"kind": "pipeline", "model": "img-m", "video_model": "vid-m", "count": "1", "duration": "5", "prompts": "一只柴犬\n一只橘猫"}
	for k, v := range fields {
		base[k] = v
	}
	for k, v := range base {
		_ = mw.WriteField(k, v)
	}
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/batches", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if w := post(s, req); w.Code != http.StatusOK {
		t.Fatalf("创建批次失败: %d %s", w.Code, w.Body)
	}
	return s.store.batches[len(s.store.batches)-1]
}

func finishImage(j *Job) {
	j.Status, j.Outputs = StatusSuccess, []string{"001_图.png"}
}

func TestPipelineCreatesImagesOnly(t *testing.T) {
	s := pipelineServer(t)
	b := createPipeline(t, s, map[string]string{"size": "16:9", "video_prompt": "镜头缓慢推进"})
	if len(b.Jobs) != 2 {
		t.Fatalf("应该只有两个图片任务，实际 %d", len(b.Jobs))
	}
	j := b.Jobs[0]
	if j.Kind != KindImage || j.Size != "1536x864" || j.VideoSize != "16:9" || j.Duration != 0 {
		t.Fatalf("图片任务不对: %+v", j)
	}
	p := b.Pipeline
	if p == nil || p.PerImage != defaultPerImage || p.Auto || p.Model != "vid-m" || p.Prompt != "镜头缓慢推进" || p.Duration != 5 {
		t.Fatalf("视频设置不对: %+v", p)
	}
}

func TestPipelineCSVVideoPrompt(t *testing.T) {
	rows, err := parseCSV([]byte("提示词,视频提示词,类型\n一只柴犬,柴犬奔跑起来,视频\n"))
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].VideoPrompt != "柴犬奔跑起来" {
		t.Fatalf("没有读到视频提示词: %+v", rows[0])
	}
}

func TestAddVideos(t *testing.T) {
	s := pipelineServer(t)
	b := createPipeline(t, s, map[string]string{"size": "9:16", "video_prompt": "通用提示词"})
	img, other := b.Jobs[0], b.Jobs[1]
	finishImage(img)
	img.VideoPrompt = "这一行自己的提示词"
	b.addVideos(img, *b.Pipeline)
	finishImage(other)
	b.addVideos(other, *b.Pipeline)

	if len(b.Jobs) != 2+2*defaultPerImage {
		t.Fatalf("任务数不对: %d", len(b.Jobs))
	}
	v := b.Jobs[2]
	if v.Kind != KindVideo || v.ParentID != img.ID || v.Status != StatusPending || v.Model != "vid-m" {
		t.Fatalf("视频任务不对: %+v", v)
	}
	if v.Prompt != "这一行自己的提示词" || b.Jobs[2+defaultPerImage].Prompt != "通用提示词" {
		t.Fatalf("提示词优先级不对: %q %q", v.Prompt, b.Jobs[2+defaultPerImage].Prompt)
	}
	if v.Image != filepath.Join(b.OutDir, "001_图.png") || v.Size != "9:16" || v.Duration != 5 {
		t.Fatalf("参考图或参数不对: %+v", v)
	}
	if !strings.HasPrefix(fileBase(*v), "001_视频1_") || !strings.HasPrefix(fileBase(*b.Jobs[3]), "001_视频2_") {
		t.Fatalf("文件名应该带图片序号: %s", fileBase(*v))
	}
}

func TestCreateVideosOnlyUsesFinishedImages(t *testing.T) {
	s := pipelineServer(t)
	b := createPipeline(t, s, nil)
	done, waiting := b.Jobs[0], b.Jobs[1]
	finishImage(done)

	send := func(ids ...string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"job_ids": ids, "model": "vid-m", "per_image": 3, "duration": 8})
		return post(s, httptest.NewRequest(http.MethodPost, "/api/batches/"+b.ID+"/videos", bytes.NewReader(body)))
	}
	if w := send(waiting.ID); w.Code != http.StatusBadRequest {
		t.Fatalf("未完成的图片不应该能生成视频: %d", w.Code)
	}
	w := send(done.ID, waiting.ID)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), strconv.Itoa(3)) {
		t.Fatalf("创建视频失败: %d %s", w.Code, w.Body)
	}
	if len(b.Jobs) != 5 || b.Jobs[4].Duration != 8 {
		t.Fatalf("应该只为完成的图片创建 3 个视频: %d", len(b.Jobs))
	}
	// 再选一次是追加，编号接着往后排，文件不会互相覆盖。
	if send(done.ID); !strings.HasPrefix(fileBase(*b.Jobs[5]), "001_视频4_") {
		t.Fatalf("追加的视频文件名重复: %s", fileBase(*b.Jobs[5]))
	}
}

func TestRegenerateBlockedAfterVideos(t *testing.T) {
	s := pipelineServer(t)
	b := createPipeline(t, s, nil)
	img, other := b.Jobs[0], b.Jobs[1]
	finishImage(img)
	finishImage(other)
	b.addVideos(other, *b.Pipeline)

	regen := func(id string) int {
		return post(s, httptest.NewRequest(http.MethodPost, "/api/jobs/"+id+"/regenerate", nil)).Code
	}
	if c := regen(img.ID); c != http.StatusOK || img.Status != StatusPending || img.Outputs != nil {
		t.Fatalf("没有视频的图片应该能重做: %d %+v", c, img)
	}
	if c := regen(other.ID); c != http.StatusBadRequest || other.Status != StatusSuccess {
		t.Fatalf("已生成视频的图片不能重做: %d", c)
	}
}

func TestImageSizeFor(t *testing.T) {
	for in, want := range map[string]string{"16:9": "1536x864", "21:9": "1680x720", "1024x1024": "1024x1024", "": ""} {
		if got := imageSizeFor(in); got != want {
			t.Errorf("imageSizeFor(%q) = %q，应为 %q", in, got, want)
		}
	}
	for ratio, size := range ratioImageSizes {
		w, h, aspect, _ := parseSize(size)
		if aspect != ratio || w%16 != 0 || h%16 != 0 {
			t.Errorf("%s 换算成 %s 后比例是 %s", ratio, size, aspect)
		}
	}
}
