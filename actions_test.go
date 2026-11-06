package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func call(s *Server, method, path string, body any) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	return post(s, httptest.NewRequest(method, path, bytes.NewReader(raw)))
}

func finishAll(b *Batch) {
	for _, j := range b.Jobs {
		if j.Status == StatusPending {
			finishImage(j)
		}
	}
}

func TestAgainJobKeepsOriginal(t *testing.T) {
	s := pipelineServer(t)
	b := createPipeline(t, s, map[string]string{"size": "16:9"})
	finishAll(b)
	orig := b.Jobs[0]
	orig.Refs = []string{"a.png"}
	if w := call(s, http.MethodPost, "/api/jobs/"+orig.ID+"/again", nil); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(b.Jobs) != 3 || orig.Status != StatusSuccess || len(orig.Outputs) != 1 {
		t.Fatalf("原任务和结果要保留: %+v", orig)
	}
	c := b.Jobs[2]
	if c.ID == orig.ID || c.Status != StatusPending || c.Outputs != nil || c.Index != 3 {
		t.Fatalf("新任务状态不对: %+v", c)
	}
	if c.Prompt != orig.Prompt || c.Model != orig.Model || c.Size != orig.Size || c.VideoSize != orig.VideoSize || len(c.Refs) != 1 {
		t.Fatalf("参数没有带过去: %+v", c)
	}
	c.Refs[0] = "changed"
	if orig.Refs[0] != "a.png" {
		t.Fatal("新任务不能和原任务共用切片")
	}
}

func TestAgainVideoKeepsImageGroupAndUniqueName(t *testing.T) {
	s := pipelineServer(t)
	b := createPipeline(t, s, nil)
	finishAll(b)
	b.addVideos(b.Jobs[0], *b.Pipeline) // 视频1–4
	v := b.Jobs[2]
	call(s, http.MethodPost, "/api/jobs/"+v.ID+"/again", nil)
	c := b.Jobs[len(b.Jobs)-1]
	if c.ParentID != v.ParentID || c.Image != v.Image || !strings.HasPrefix(fileBase(*c), "001_视频5_") {
		t.Fatalf("再次生成的视频要归到同一张图、文件名不重复: %+v %s", c, fileBase(*c))
	}
}

func TestAgainBatchSkipsVideos(t *testing.T) {
	s := pipelineServer(t)
	b := createPipeline(t, s, nil)
	finishAll(b)
	b.addVideos(b.Jobs[0], *b.Pipeline)
	before := len(b.Jobs)
	w := call(s, http.MethodPost, "/api/batches/"+b.ID+"/again", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"count":2`) || len(b.Jobs) != before+2 {
		t.Fatalf("整批再次生成只复制两张图片: %s %d", w.Body, len(b.Jobs))
	}
	if j := b.Jobs[len(b.Jobs)-1]; j.Kind != KindImage || j.ParentID != "" {
		t.Fatalf("复制的应该是图片: %+v", j)
	}
}

func TestDeletedIndexIsNeverReused(t *testing.T) {
	// 回归：删除只删记录，文件还在。序号重复使用会让新文件覆盖旧文件。
	s := pipelineServer(t)
	b := createPipeline(t, s, nil)
	finishAll(b)
	last := b.Jobs[1]
	first := b.Jobs[0]
	first.VideoSeq = 0
	if w := call(s, http.MethodPost, "/api/batches/"+b.ID+"/jobs/delete", map[string]any{"job_ids": []string{last.ID}}); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	call(s, http.MethodPost, "/api/jobs/"+first.ID+"/again", nil)
	if c := b.Jobs[len(b.Jobs)-1]; c.Index != 3 {
		t.Fatalf("序号 2 已经用过，应该是 3，实际 %d", c.Index)
	}

	// 视频同理：删掉视频4 后再生成，不能又叫视频4。
	b.addVideos(first, *b.Pipeline)
	var four string
	for _, j := range b.Jobs {
		if strings.HasPrefix(j.Stem, "001_视频4_") {
			four = j.ID
			j.Status = StatusSuccess
		}
	}
	call(s, http.MethodPost, "/api/batches/"+b.ID+"/jobs/delete", map[string]any{"job_ids": []string{four}})
	b.addVideos(first, Pipeline{Model: "m", PerImage: 1})
	if c := b.Jobs[len(b.Jobs)-1]; !strings.HasPrefix(c.Stem, "001_视频5_") {
		t.Fatalf("视频序号不能重复: %s", c.Stem)
	}
}

func TestDeleteJobsSkipsActiveAndOrphansVideos(t *testing.T) {
	s := pipelineServer(t)
	b := createPipeline(t, s, nil)
	finishImage(b.Jobs[0])
	img, queued := b.Jobs[0], b.Jobs[1]
	b.addVideos(img, Pipeline{Model: "m", PerImage: 1})
	video := b.Jobs[2]
	video.Status = StatusSuccess

	w := call(s, http.MethodPost, "/api/batches/"+b.ID+"/jobs/delete", map[string]any{"job_ids": []string{img.ID, queued.ID}})
	if !strings.Contains(w.Body.String(), `"deleted":1`) || !strings.Contains(w.Body.String(), `"skipped":1`) {
		t.Fatalf("排队中的任务不能删: %s", w.Body)
	}
	if len(b.Jobs) != 2 || video.ParentID != "" {
		t.Fatalf("来源图片删除后视频要保留并脱离分组: %+v", video)
	}
}

func TestDeleteAllJobsRemovesBatch(t *testing.T) {
	s := pipelineServer(t)
	b := createPipeline(t, s, nil)
	finishAll(b)
	call(s, http.MethodPost, "/api/batches/"+b.ID+"/jobs/delete", map[string]any{"job_ids": []string{b.Jobs[0].ID, b.Jobs[1].ID}})
	if len(s.store.batches) != 0 {
		t.Fatal("任务删空后批次也要删除")
	}
}

func TestDeleteBatchesSkipsRunning(t *testing.T) {
	s := pipelineServer(t)
	idle := createPipeline(t, s, nil)
	finishAll(idle)
	running := createPipeline(t, s, nil)
	ref := filepath.Join(s.dataDir, "refs", idle.ID+".png")
	os.MkdirAll(filepath.Dir(ref), 0o755)
	os.WriteFile(ref, []byte("x"), 0o644)
	w := call(s, http.MethodPost, "/api/batches/delete", map[string]any{"ids": []string{idle.ID, running.ID}})
	if !strings.Contains(w.Body.String(), `"deleted":1`) || !strings.Contains(w.Body.String(), `"skipped":1`) {
		t.Fatalf("%s", w.Body)
	}
	if len(s.store.batches) != 1 || s.store.batches[0].ID != running.ID {
		t.Fatal("进行中的批次不能删")
	}
	if _, err := os.Stat(ref); !os.IsNotExist(err) {
		t.Fatal("参考图要一起清理")
	}
}

func TestJobRef(t *testing.T) {
	s := pipelineServer(t)
	b := createPipeline(t, s, nil)
	dir := t.TempDir()
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)
	a, c, l := filepath.Join(dir, "a.png"), filepath.Join(dir, "c.png"), filepath.Join(dir, "l.png")
	for _, p := range []string{a, c, l} {
		os.WriteFile(p, png, 0o644)
	}
	j := b.Jobs[0]
	j.Image, j.Refs, j.LastImage = a, []string{c}, l
	get := func(n string) int {
		return call(s, http.MethodGet, "/api/jobs/"+j.ID+"/refs/"+n, nil).Code
	}
	for n, want := range map[string]int{"0": 200, "1": 200, "last": 200, "2": 404, "-1": 404, "x": 404} {
		if got := get(n); got != want {
			t.Errorf("refs/%s = %d，应为 %d", n, got, want)
		}
	}
	os.Remove(c)
	if got := get("1"); got != 404 {
		t.Errorf("文件没了应该 404，实际 %d", got)
	}
}
