package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// defaultPerImage 是每张图默认生成的视频数量。
const defaultPerImage = 4

// Pipeline 是「图转视频」批次的视频设置。视频任务在图片被选中时才创建
// （全自动时在图片完成时创建），所以图片失败或取消不会产生任何视频任务。
type Pipeline struct {
	Prompt   string `json:"prompt,omitempty"` // 通用视频提示词；单行写了 VideoPrompt 的优先
	Model    string `json:"model"`
	Size     string `json:"size,omitempty"`
	Duration int    `json:"duration,omitempty"`
	Mode     string `json:"mode,omitempty"`
	PerImage int    `json:"per_image"`
	Auto     bool   `json:"auto,omitempty"` // true = 不挑图，图片完成后直接生成视频
}

// ratioImageSizes 把比例换算成图片尺寸，保证图片和视频画幅一致。边长都是 16 的倍数。
var ratioImageSizes = map[string]string{
	"16:9": "1536x864", "9:16": "864x1536", "1:1": "1024x1024",
	"4:3": "1152x864", "3:4": "864x1152", "21:9": "1680x720",
	"3:2": "1536x1024", "2:3": "1024x1536",
}

func imageSizeFor(size string) string {
	if s, ok := ratioImageSizes[size]; ok {
		return s
	}
	return size
}

func (b *Batch) videoCount(imageID string) int {
	n := 0
	for _, j := range b.Jobs {
		if j.ParentID == imageID {
			n++
		}
	}
	return n
}

// addVideos 用一张已完成的图片创建 p.PerImage 个视频任务，调用方需持有 store 锁。
func (b *Batch) addVideos(img *Job, p Pipeline) {
	now := time.Now()
	prompt := firstNonEmpty(img.VideoPrompt, p.Prompt, img.Prompt)
	for range p.PerImage {
		b.Jobs = append(b.Jobs, &Job{
			ID: newID(), Index: b.nextIndex(), Kind: KindVideo,
			Prompt: prompt, Model: p.Model, Size: firstNonEmpty(img.VideoSize, p.Size), Duration: p.Duration,
			Image: filepath.Join(b.OutDir, img.Outputs[0]), Mode: p.Mode, ParentID: img.ID,
			Stem:   b.videoStem(img, prompt),
			Status: StatusPending, UpdatedAt: now,
		})
	}
}

// videoStem 分配视频文件名：带上图片序号，文件夹里图片和它的视频排在一起。序号不重复使用，以免覆盖已删除记录的文件。
func (b *Batch) videoStem(img *Job, prompt string) string {
	img.VideoSeq = max(img.VideoSeq, b.videoCount(img.ID)) + 1
	return fmt.Sprintf("%03d_视频%d_%s", img.Index, img.VideoSeq, sanitize(prompt, 24))
}

// createVideos：用户勾选了图片后，为每张图创建视频任务。
func (s *Server) createVideos(w http.ResponseWriter, r *http.Request) {
	var req struct {
		JobIDs []string `json:"job_ids"`
		Pipeline
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		httpErr(w, http.StatusBadRequest, "请求格式错误: "+err.Error())
		return
	}
	p := req.Pipeline
	p.Prompt, p.Size = strings.TrimSpace(p.Prompt), strings.TrimSpace(p.Size)
	p.Model = firstNonEmpty(strings.TrimSpace(p.Model), s.cfg.Get().VideoModel)
	p.Mode = parseMode(p.Mode)
	p.PerImage = clampInt(p.PerImage, 1, 20)
	if p.Model == "" {
		httpErr(w, http.StatusBadRequest, "请选择视频模型")
		return
	}

	status, msg, n := http.StatusOK, "", 0
	s.store.Update(func() bool {
		b := s.store.batchLocked(r.PathValue("id"))
		if b == nil {
			status, msg = http.StatusNotFound, "批次不存在"
			return false
		}
		var images []*Job
		for _, j := range b.Jobs {
			if slices.Contains(req.JobIDs, j.ID) && j.Kind == KindImage && j.Status == StatusSuccess && len(j.Outputs) > 0 {
				images = append(images, j)
			}
		}
		if len(images) == 0 {
			status, msg = http.StatusBadRequest, "没有可用的图片，请勾选已完成的图片"
			return false
		}
		if len(b.Jobs)+len(images)*p.PerImage > maxJobsPerBatch {
			status, msg = http.StatusBadRequest, fmt.Sprintf("单个批次最多 %d 个任务", maxJobsPerBatch)
			return false
		}
		for _, img := range images {
			b.addVideos(img, p)
		}
		n = len(images) * p.PerImage
		return true
	})
	if msg != "" {
		httpErr(w, status, msg)
		return
	}
	s.runner.Wake()
	writeJSON(w, http.StatusOK, map[string]int{"count": n})
}

// regenerateJob 重新生成一张不满意的图片。已经用来生成视频的图片不能重做，否则图片和视频对不上。
func (s *Server) regenerateJob(w http.ResponseWriter, r *http.Request) {
	msg := "该图片当前不能重新生成"
	s.store.Update(func() bool {
		b, j := s.store.jobLocked(r.PathValue("id"))
		if j == nil || j.Kind != KindImage || j.Status != StatusSuccess {
			return false
		}
		if b.videoCount(j.ID) > 0 {
			msg = "这张图片已经生成了视频，不能重新生成"
			return false
		}
		j.reset()
		msg = ""
		return true
	})
	if msg != "" {
		httpErr(w, http.StatusBadRequest, msg)
		return
	}
	s.runner.Wake()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func kindLabel(kind string) string {
	if kind == KindVideo {
		return "视频"
	}
	return "图片"
}
