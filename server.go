package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"io/fs"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const maxJobsPerBatch = 2000

type Server struct {
	store   *Store
	cfg     *ConfigStore
	runner  *Runner
	updater *Updater
	dataDir string
	static  fs.FS
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(s.static))
	mux.HandleFunc("GET /api/ping", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"app": "batchgen", "version": appVersion})
	})
	mux.HandleFunc("POST /api/shutdown", s.shutdown)
	mux.HandleFunc("GET /api/update", s.updateStatus)
	mux.HandleFunc("POST /api/update/check", s.updateCheck)
	mux.HandleFunc("POST /api/update/apply", s.updateApply)
	mux.HandleFunc("GET /api/config", s.getConfig)
	mux.HandleFunc("PUT /api/config", s.putConfig)
	mux.HandleFunc("POST /api/test", s.testConn)
	mux.HandleFunc("GET /api/presets", s.listPresets)
	mux.HandleFunc("POST /api/preview", s.preview)
	mux.HandleFunc("GET /api/batches", s.listBatches)
	mux.HandleFunc("POST /api/batches", s.createBatch)
	mux.HandleFunc("POST /api/batches/{id}/retry", s.retryBatch)
	mux.HandleFunc("POST /api/batches/{id}/videos", s.createVideos)
	mux.HandleFunc("POST /api/batches/{id}/again", s.againBatch)
	mux.HandleFunc("POST /api/batches/{id}/jobs/delete", s.deleteJobs)
	mux.HandleFunc("POST /api/batches/delete", s.deleteBatches)
	mux.HandleFunc("POST /api/batches/{id}/cancel", s.cancelBatch)
	mux.HandleFunc("POST /api/batches/{id}/open", s.openBatch)
	mux.HandleFunc("DELETE /api/batches/{id}", s.deleteBatch)
	mux.HandleFunc("POST /api/jobs/{id}/retry", s.retryJob)
	mux.HandleFunc("POST /api/jobs/{id}/regenerate", s.regenerateJob)
	mux.HandleFunc("POST /api/jobs/{id}/again", s.againJob)
	mux.HandleFunc("GET /api/jobs/{id}/refs/{n}", s.jobRef)
	mux.HandleFunc("POST /api/jobs/{id}/cancel", s.cancelJob)
	mux.HandleFunc("POST /api/jobs/{id}/reveal", s.revealJob)
	mux.HandleFunc("POST /api/jobs/{id}/fix", s.fixJob)
	mux.HandleFunc("POST /api/open-output", s.openOutput)
	mux.HandleFunc("POST /api/test-upload", s.testUpload)
	mux.HandleFunc("GET /files/{batch}/{name}", s.serveFile)
	return guard(mux)
}

// guard 防止其它网页借用户的浏览器操作本地服务：
// 校验 Host（防 DNS 重绑定），写操作要求自定义请求头（跨站请求会被 CORS 预检拦下）。
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if host != "127.0.0.1" && host != "localhost" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-BatchGen") != "1" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		// 页面据此发现后台换了新版本并自动刷新；no-cache 让浏览器每次都确认文件是否更新。
		w.Header().Set("X-BatchGen-Version", appVersion)
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// shutdown 供新版本接管：没有任务在跑时退出，让出端口；有任务在跑时拒绝，避免中断生成。
func (s *Server) shutdown(w http.ResponseWriter, r *http.Request) {
	if n := s.runner.ActiveCount(); n > 0 {
		httpErr(w, http.StatusConflict, fmt.Sprintf("旧版本还有 %d 个任务正在生成。请等任务完成（或在任务页取消）后，关闭旧版本的窗口，再打开新版本", n))
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	go func() {
		time.Sleep(300 * time.Millisecond)
		os.Exit(0)
	}()
}

type configView struct {
	Config
	OutputRoot string `json:"output_root"`
	// 当前接口格式能否一次发多张参考图，新建页据此显示上传上限。
	ImageManyRefs bool `json:"image_many_refs"`
	VideoManyRefs bool `json:"video_many_refs"`
}

func (s *Server) view(c Config) configView {
	return configView{c, s.cfg.OutputRoot(c), c.ImageSpec.acceptsManyRefs(), c.VideoSpec.acceptsManyRefs()}
}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.view(s.cfg.Get()))
}

func (s *Server) putConfig(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		httpErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// 在现有配置上覆盖，请求里没带的字段保持原值；带了接口格式的话整份替换（否则请求头等 map 会被合并）。
	var present map[string]json.RawMessage
	if err := json.Unmarshal(body, &present); err != nil {
		httpErr(w, http.StatusBadRequest, "配置格式错误: "+err.Error())
		return
	}
	c := s.cfg.Get()
	if _, ok := present["image_spec"]; ok {
		c.ImageSpec = Spec{}
	}
	if _, ok := present["video_spec"]; ok {
		c.VideoSpec = Spec{}
	}
	if err := json.Unmarshal(body, &c); err != nil {
		httpErr(w, http.StatusBadRequest, "配置格式错误: "+err.Error())
		return
	}
	c.normalize()
	if err := c.validate(); err != nil {
		httpErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.cfg.Set(c); err != nil {
		httpErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	s.runner.Wake()
	writeJSON(w, http.StatusOK, s.view(c))
}

func (s *Server) testConn(w http.ResponseWriter, r *http.Request) {
	var c Config
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&c); err != nil {
		httpErr(w, http.StatusBadRequest, err.Error())
		return
	}
	c.normalize()
	if c.BaseURL == "" || len(c.APIKeys) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "请先填写接口地址和 API Key"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	models, err := NewClient(c.BaseURL, c.APIKeys[0], "bearer", nil).ListModels(ctx, c.ModelsPath)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": models})
}

func (s *Server) listPresets(w http.ResponseWriter, r *http.Request) {
	list := make([]Preset, 0, len(presets)+2)
	for _, p := range presets {
		p.Spec, _ = presetByID(p.ID)
		list = append(list, p)
	}
	for _, kind := range []string{KindImage, KindVideo} {
		spec := customTemplate.clone()
		spec.normalize()
		list = append(list, Preset{ID: "custom", Kind: kind, Name: "自定义接口",
			Desc: "自己填写请求路径、请求体模板和返回字段。也可以先选一个接近的预设，再在下面修改。", Spec: spec})
	}
	writeJSON(w, http.StatusOK, list)
}

// preview 用示例数据渲染一次提交请求，方便核对自定义接口，不会真的发出请求。
func (s *Server) preview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind    string       `json:"kind"`
		Spec    Spec         `json:"spec"`
		BaseURL string       `json:"base_url"`
		Model   string       `json:"model"`
		WithRef bool         `json:"with_ref"`
		Extras  []ExtraParam `json:"extra_params"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		httpErr(w, http.StatusBadRequest, err.Error())
		return
	}
	spec := req.Spec
	spec.normalize()
	in := GenInput{Kind: req.Kind, Model: firstNonEmpty(strings.TrimSpace(req.Model), "示例模型"),
		Prompt: "一只在雨中奔跑的柴犬，电影感", Size: "1280x720"}
	if req.Kind == KindVideo {
		in.Duration = 5
	}
	if req.WithRef {
		in.Ref = &RefImage{URL: "https://example.com/reference.png", Mime: "image/png"}
	}
	in.Extras = extrasFor(normExtraParams(req.Extras), req.Kind, in.Model)
	p, err := prepare(&spec, in, true)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	base := firstNonEmpty(spec.BaseURL, normBase(req.BaseURL), "<接口地址>")
	target := p.Path
	if !isHTTP(target) {
		target = base + target
	}
	headers := []string{}
	switch spec.Auth {
	case "x-goog-api-key", "x-api-key":
		headers = append(headers, spec.Auth+": <API Key>")
	case "kling_jwt":
		headers = append(headers, "Authorization: Bearer <JWT 或 API Key>")
	case "none":
	default:
		headers = append(headers, "Authorization: Bearer <API Key>")
	}
	for k, v := range spec.Headers {
		headers = append(headers, k+": "+v)
	}
	if p.ContentType != "" {
		headers = append(headers, "Content-Type: "+strings.SplitN(p.ContentType, ";", 2)[0])
	}
	text := p.Method + " " + target + "\n" + strings.Join(headers, "\n") + "\n\n" + p.Preview
	if spec.TaskID != "" {
		qp := spec.Poll.Path
		if p.UsedRef && spec.Poll.PathRef != "" {
			qp = spec.Poll.PathRef
		}
		qp = strings.ReplaceAll(renderPath(qp, map[string]string{"model": in.Model}), "{id}", "<任务ID>")
		if !isHTTP(qp) {
			qp = base + qp
		}
		text += "\n\n# 之后轮询：GET " + qp
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "text": text})
}

func (s *Server) listBatches(w http.ResponseWriter, r *http.Request) {
	data, err := s.store.MarshalBatches()
	if err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(data)
}

// saveUpload 把上传的图片存到 dir/name.扩展名，返回保存路径；没上传返回空。
// saveRefs 保存上传的参考图，返回本地路径。
func saveRefs(r *http.Request, dir, batchID string, limit int) ([]string, error) {
	var paths []string
	files := slices.DeleteFunc(slices.Clone(r.MultipartForm.File["ref"]), func(h *multipart.FileHeader) bool { return h.Size == 0 })
	if len(files) > limit {
		return nil, fmt.Errorf("最多上传 %d 张（可在「设置 → 运行与存储」里调整）", limit)
	}
	for i, h := range files {
		if h.Size > 50<<20 {
			return nil, fmt.Errorf("文件 %s 太大", h.Filename)
		}
		f, err := h.Open()
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			return nil, err
		}
		ref, err := newRef("", data)
		if err != nil {
			return nil, fmt.Errorf("%s：%w", h.Filename, err)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		name := batchID
		if i > 0 {
			name = fmt.Sprintf("%s_%d", batchID, i+1)
		}
		p := filepath.Join(dir, name+extForMime(ref.Mime, ".png"))
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	return paths, nil
}

func saveUpload(r *http.Request, field, dir, name string) (string, error) {
	data, ok, err := formFile(r, field, 50<<20)
	if err != nil || !ok {
		return "", err
	}
	ref, err := newRef("", data)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(dir, name+extForMime(ref.Mime, ".png"))
	return p, os.WriteFile(p, data, 0o644)
}

func formFile(r *http.Request, field string, limit int64) ([]byte, bool, error) {
	f, h, err := r.FormFile(field)
	if errors.Is(err, http.ErrMissingFile) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	if h.Size == 0 {
		return nil, false, nil
	}
	if h.Size > limit {
		return nil, false, fmt.Errorf("文件 %s 太大", h.Filename)
	}
	data, err := io.ReadAll(f)
	return data, err == nil, err
}

func (s *Server) createBatch(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 512<<20)
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		httpErr(w, http.StatusBadRequest, "表单解析失败: "+err.Error())
		return
	}
	cfg := s.cfg.Get()
	def := Row{
		Kind:  r.FormValue("kind"),
		Model: strings.TrimSpace(r.FormValue("model")),
		Size:  strings.TrimSpace(r.FormValue("size")),
	}
	def.Duration, _ = strconv.Atoi(r.FormValue("duration"))
	def.Count, _ = strconv.Atoi(r.FormValue("count"))
	var pipe *Pipeline
	if def.Kind == "pipeline" {
		perImage, err := strconv.Atoi(r.FormValue("per_image"))
		if err != nil {
			perImage = defaultPerImage
		}
		pipe = &Pipeline{
			Prompt: strings.TrimSpace(r.FormValue("video_prompt")), Size: def.Size, Duration: def.Duration,
			Model: firstNonEmpty(strings.TrimSpace(r.FormValue("video_model")), cfg.VideoModel),
			Mode:  parseMode(r.FormValue("mode")), PerImage: clampInt(perImage, 1, 20), Auto: r.FormValue("auto") != "",
		}
		if pipe.Model == "" {
			httpErr(w, http.StatusBadRequest, "请选择视频模型")
			return
		}
	}
	if def.Kind != KindVideo {
		def.Kind = KindImage
	}

	var rows []Row
	csvData, hasCSV, err := formFile(r, "csv", 10<<20)
	if err != nil {
		httpErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if hasCSV {
		if rows, err = parseCSV(csvData); err != nil {
			httpErr(w, http.StatusBadRequest, err.Error())
			return
		}
	} else {
		rows = parseLines(r.FormValue("prompts"))
	}
	if len(rows) == 0 {
		httpErr(w, http.StatusBadRequest, "没有可用的提示词")
		return
	}

	batchID := newID()
	refPaths, err := saveRefs(r, filepath.Join(s.dataDir, "refs"), batchID, cfg.MaxRefImages)
	if err != nil {
		httpErr(w, http.StatusBadRequest, "参考图："+err.Error())
		return
	}
	refPath := ""
	var refExtra []string
	if len(refPaths) > 0 {
		refPath, refExtra = refPaths[0], refPaths[1:]
	}
	lastPath, err := saveUpload(r, "ref_last", filepath.Join(s.dataDir, "refs"), batchID+"_last")
	if err != nil {
		httpErr(w, http.StatusBadRequest, "尾帧："+err.Error())
		return
	}
	def.Mode = parseMode(r.FormValue("mode"))
	if def.Mode == "frames" && refPath == "" && !hasCSV && pipe == nil {
		httpErr(w, http.StatusBadRequest, "首尾帧模式需要上传首帧图片")
		return
	}

	now := time.Now()
	var jobs []*Job
	for i, row := range rows {
		kind := firstNonEmpty(row.Kind, def.Kind)
		if pipe != nil {
			kind = KindImage // 图转视频的每一行都先出图
		}
		model := firstNonEmpty(row.Model, def.Model, cfg.defaultModel(kind))
		if model == "" {
			httpErr(w, http.StatusBadRequest, fmt.Sprintf("第 %d 条没有指定模型", i+1))
			return
		}
		dur := row.Duration
		if dur == 0 {
			dur = def.Duration
		}
		if kind == KindImage {
			dur = 0
		}
		size, videoSize := firstNonEmpty(row.Size, def.Size), ""
		if pipe != nil {
			videoSize = size
		}
		if kind == KindImage {
			size = imageSizeFor(size) // 图片接口要具体尺寸，比例换算成像素
		}
		count := row.Count
		if count <= 0 {
			count = def.Count
		}
		count = clampInt(count, 1, 20)
		mode, last := "", ""
		if kind == KindVideo {
			mode = firstNonEmpty(row.Mode, def.Mode)
			last = firstNonEmpty(row.LastImage, lastPath)
			if firstNonEmpty(row.Image, refPath) == "" {
				last = "" // 没有首帧时尾帧没有意义
			}
		}
		extra := refExtra
		if row.Image != "" {
			extra = row.Refs
		}
		if n := len(extra) + 1; n > 1 {
			if n > cfg.MaxRefImages {
				httpErr(w, http.StatusBadRequest, fmt.Sprintf("第 %d 条有 %d 张参考图，超过上限 %d 张（可在「设置 → 运行与存储」里调整）", i+1, n, cfg.MaxRefImages))
				return
			}
			if !cfg.Spec(kind).acceptsManyRefs() {
				httpErr(w, http.StatusBadRequest, fmt.Sprintf("当前的%s接口格式一次只能用 1 张参考图。要用多张，请到「设置」换成支持多图的格式（视频用「OpenAI 视频 · Seedance 扩展」）", kindLabel(kind)))
				return
			}
			if mode == "frames" {
				httpErr(w, http.StatusBadRequest, "首尾帧模式只用 1 张首帧，请减少参考图或改用全能参考")
				return
			}
		}
		for range count {
			jobs = append(jobs, &Job{
				ID: newID(), Index: len(jobs) + 1, Kind: kind,
				Prompt: row.Prompt, Model: model, Size: size,
				VideoPrompt: row.VideoPrompt, VideoSize: videoSize,
				Duration: dur, Image: firstNonEmpty(row.Image, refPath), Refs: extra,
				Mode: mode, LastImage: last,
				Status: StatusPending, UpdatedAt: now,
			})
		}
		if len(jobs) > maxJobsPerBatch {
			httpErr(w, http.StatusBadRequest, fmt.Sprintf("单个批次最多 %d 个任务", maxJobsPerBatch))
			return
		}
	}

	if pipe != nil && len(jobs)*(1+pipe.PerImage) > maxJobsPerBatch {
		httpErr(w, http.StatusBadRequest, fmt.Sprintf("单个批次最多 %d 个任务（图片加视频），请减少提示词或每张图的视频数", maxJobsPerBatch))
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		name = clip(rows[0].Prompt, 24)
	}
	outDir := filepath.Join(s.cfg.OutputRoot(cfg), now.Format("20060102_150405")+"_"+sanitize(name, 24)+"_"+batchID[:4])
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		httpErr(w, http.StatusInternalServerError, "无法创建输出目录: "+err.Error())
		return
	}

	b := &Batch{ID: batchID, Name: name, OutDir: outDir, CreatedAt: now, Jobs: jobs, Pipeline: pipe}
	s.store.Update(func() bool {
		s.store.batches = append(s.store.batches, b)
		return true
	})
	s.runner.Wake()
	writeJSON(w, http.StatusOK, map[string]any{"id": batchID, "count": len(jobs)})
}

// resetIfRetryable 把失败/取消且已停止运行的任务放回队列。
func (s *Server) resetIfRetryable(j *Job) bool {
	if (j.Status == StatusFailed || j.Status == StatusCanceled) && !s.runner.IsActive(j.ID) {
		j.reset()
		return true
	}
	return false
}

func (s *Server) retryBatch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	found, n := false, 0
	s.store.Update(func() bool {
		b := s.store.batchLocked(id)
		if b == nil {
			return false
		}
		found = true
		for _, j := range b.Jobs {
			if s.resetIfRetryable(j) {
				n++
			}
		}
		return n > 0
	})
	if !found {
		httpErr(w, http.StatusNotFound, "批次不存在")
		return
	}
	s.runner.Wake()
	writeJSON(w, http.StatusOK, map[string]int{"count": n})
}

func (s *Server) retryJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ok := false
	s.store.Update(func() bool {
		if _, j := s.store.jobLocked(id); j != nil {
			ok = s.resetIfRetryable(j)
		}
		return ok
	})
	if !ok {
		httpErr(w, http.StatusBadRequest, "该任务当前不能重试")
		return
	}
	s.runner.Wake()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) cancelBatch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	n := s.runner.Cancel(func(b *Batch, j *Job) bool { return b.ID == id })
	writeJSON(w, http.StatusOK, map[string]int{"count": n})
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	n := s.runner.Cancel(func(b *Batch, j *Job) bool { return j.ID == id })
	writeJSON(w, http.StatusOK, map[string]int{"count": n})
}

func (s *Server) openBatch(w http.ResponseWriter, r *http.Request) {
	var dir string
	s.store.View(func() {
		if b := s.store.batchLocked(r.PathValue("id")); b != nil {
			dir = b.OutDir
		}
	})
	if dir == "" {
		httpErr(w, http.StatusNotFound, "批次不存在")
		return
	}
	if err := openFolder(dir); err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// fixJob：用户在失败卡片上选了参数值。记成附加参数规则（只作用于这个模型），
// 再把本批次里同类型、同模型的失败任务放回队列重试。
func (s *Server) fixJob(w http.ResponseWriter, r *http.Request) {
	var req struct{ Param, Value string }
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil || strings.TrimSpace(req.Param) == "" {
		httpErr(w, http.StatusBadRequest, "参数名不能为空")
		return
	}
	var kind, model, batchID string
	s.store.View(func() {
		if b, j := s.store.jobLocked(r.PathValue("id")); j != nil {
			kind, model, batchID = j.Kind, j.Model, b.ID
		}
	})
	if batchID == "" {
		httpErr(w, http.StatusNotFound, "任务不存在")
		return
	}

	c := s.cfg.Get()
	c.ExtraParams = append(c.ExtraParams, ExtraParam{Kind: kind, Model: model, Key: req.Param, Value: req.Value})
	c.normalize()
	if err := s.cfg.Set(c); err != nil {
		httpErr(w, http.StatusInternalServerError, "保存设置失败: "+err.Error())
		return
	}

	n := 0
	s.store.Update(func() bool {
		if b := s.store.batchLocked(batchID); b != nil {
			for _, j := range b.Jobs {
				if j.Kind == kind && j.Model == model && s.resetIfRetryable(j) {
					n++
				}
			}
		}
		return n > 0
	})
	s.runner.Wake()
	writeJSON(w, http.StatusOK, map[string]any{"count": n, "model": model})
}

// testUpload 用一张 8x8 的小图测试图床：上传后再用公开网址取回，确认真的能访问。
func (s *Server) testUpload(w http.ResponseWriter, r *http.Request) {
	var h ImageHost
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&h); err != nil {
		httpErr(w, http.StatusBadRequest, err.Error())
		return
	}
	h.normalize()
	if h.Mode == "off" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "参考图上传已关闭"})
		return
	}
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	seed := byte(time.Now().UnixNano())
	for i := range img.Pix {
		img.Pix[i] = byte(i) ^ seed
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)

	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	link, err := NewUploader().Upload(ctx, h, &RefImage{Data: buf.Bytes(), Mime: "image/png"})
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	resp, err := httpClient.Do(req)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "上传成功，但用公开网址取不回来：" + err.Error(), "url": link})
		return
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": fmt.Sprintf("上传成功，但公开网址返回 HTTP %d（检查「公开访问地址」或存储的公开权限）", resp.StatusCode), "url": link})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "url": link})
}

// revealJob 在资源管理器里打开结果所在文件夹并选中该文件。
func (s *Server) revealJob(w http.ResponseWriter, r *http.Request) {
	var file string
	s.store.View(func() {
		if b, j := s.store.jobLocked(r.PathValue("id")); j != nil && len(j.Outputs) > 0 {
			file = filepath.Join(b.OutDir, j.Outputs[0])
		}
	})
	if file == "" {
		httpErr(w, http.StatusNotFound, "这个任务还没有生成文件")
		return
	}
	if _, err := os.Stat(file); err != nil {
		httpErr(w, http.StatusNotFound, "文件不存在，可能已被移动或删除："+file)
		return
	}
	if err := revealFile(file); err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) openOutput(w http.ResponseWriter, r *http.Request) {
	root := s.cfg.OutputRoot(s.cfg.Get())
	if err := os.MkdirAll(root, 0o755); err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := openFolder(root); err != nil {
		httpErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) deleteBatch(w http.ResponseWriter, r *http.Request) {
	if err := s.dropBatch(r.PathValue("id")); err != nil {
		httpErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request) {
	var dir string
	s.store.View(func() {
		if b := s.store.batchLocked(r.PathValue("batch")); b != nil {
			dir = b.OutDir
		}
	})
	name := r.PathValue("name")
	if dir == "" || !filepath.IsLocal(name) || strings.ContainsAny(name, `/\`) {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, filepath.Join(dir, name))
}
