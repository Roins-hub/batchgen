package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"
)

// nextIndex 分配任务序号。序号只增不减：删除任务时文件留在磁盘上，重复使用序号会把旧文件覆盖掉。
func (b *Batch) nextIndex() int {
	b.syncIndex()
	b.LastIndex++
	return b.LastIndex
}

func (b *Batch) syncIndex() {
	for _, j := range b.Jobs {
		b.LastIndex = max(b.LastIndex, j.Index)
	}
}

// addCopy 按原任务的参数在批次里新建一个排队中的任务，原任务和它的结果不动。
func (b *Batch) addCopy(j *Job) *Job {
	c := &Job{
		ID: newID(), Index: b.nextIndex(), Kind: j.Kind, Prompt: j.Prompt, Model: j.Model, Size: j.Size, Duration: j.Duration,
		Image: j.Image, Refs: slices.Clone(j.Refs), Mode: j.Mode, LastImage: j.LastImage,
		VideoPrompt: j.VideoPrompt, VideoSize: j.VideoSize, ParentID: j.ParentID,
		Status: StatusPending, UpdatedAt: time.Now(),
	}
	if _, parent := b.findJob(j.ParentID); parent != nil {
		c.Stem = b.videoStem(parent, c.Prompt)
	}
	b.Jobs = append(b.Jobs, c)
	return c
}

func (b *Batch) findJob(id string) (int, *Job) {
	i := slices.IndexFunc(b.Jobs, func(j *Job) bool { return j.ID == id })
	if i < 0 {
		return -1, nil
	}
	return i, b.Jobs[i]
}

func (j *Job) busy() bool {
	switch j.Status {
	case StatusPending, StatusSubmitting, StatusRunning, StatusDownloading:
		return true
	}
	return false
}

// againJob：在同一批次里按相同参数再生成一次。
func (s *Server) againJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	status, msg := http.StatusOK, ""
	s.store.Update(func() bool {
		b, j := s.store.jobLocked(id)
		switch {
		case j == nil:
			status, msg = http.StatusNotFound, "任务不存在"
		case len(b.Jobs) >= maxJobsPerBatch:
			status, msg = http.StatusBadRequest, fmt.Sprintf("单个批次最多 %d 个任务", maxJobsPerBatch)
		default:
			b.addCopy(j)
			return true
		}
		return false
	})
	if msg != "" {
		httpErr(w, status, msg)
		return
	}
	s.runner.Wake()
	writeJSON(w, http.StatusOK, map[string]int{"count": 1})
}

// againBatch：整批再生成一次。图转视频的视频不复制，新图片完成后按批次设置继续。
func (s *Server) againBatch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	status, msg, n := http.StatusOK, "", 0
	s.store.Update(func() bool {
		b := s.store.batchLocked(id)
		if b == nil {
			status, msg = http.StatusNotFound, "批次不存在"
			return false
		}
		var src []*Job
		for _, j := range b.Jobs {
			if j.ParentID == "" {
				src = append(src, j)
			}
		}
		if len(b.Jobs)+len(src) > maxJobsPerBatch {
			status, msg = http.StatusBadRequest, fmt.Sprintf("单个批次最多 %d 个任务", maxJobsPerBatch)
			return false
		}
		for _, j := range src {
			b.addCopy(j)
		}
		n = len(src)
		return n > 0
	})
	if msg != "" {
		httpErr(w, status, msg)
		return
	}
	s.runner.Wake()
	writeJSON(w, http.StatusOK, map[string]int{"count": n})
}

// dropBatch 删除批次记录。生成的文件保留，只清理本批次上传的参考图。
func (s *Server) dropBatch(id string) error {
	var err error
	s.store.Update(func() bool {
		idx := slices.IndexFunc(s.store.batches, func(b *Batch) bool { return b.ID == id })
		if idx < 0 {
			err = errors.New("批次不存在")
			return false
		}
		for _, j := range s.store.batches[idx].Jobs {
			if j.busy() || s.runner.IsActive(j.ID) {
				err = errors.New("该批次还有任务在运行，请先取消")
				return false
			}
		}
		s.store.batches = slices.Delete(s.store.batches, idx, idx+1)
		return true
	})
	if err != nil {
		return err
	}
	if refs, _ := filepath.Glob(filepath.Join(s.dataDir, "refs", id+"*")); refs != nil {
		for _, p := range refs {
			os.Remove(p)
		}
	}
	return nil
}

func (s *Server) deleteBatches(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		httpErr(w, http.StatusBadRequest, "请求格式错误: "+err.Error())
		return
	}
	deleted := 0
	for _, id := range req.IDs {
		if s.dropBatch(id) == nil {
			deleted++
		}
	}
	writeJSON(w, http.StatusOK, map[string]int{"deleted": deleted, "skipped": len(req.IDs) - deleted})
}

// deleteJobs 删除选中的任务记录，文件保留；进行中的任务跳过。批次删空后一起删除。
func (s *Server) deleteJobs(w http.ResponseWriter, r *http.Request) {
	var req struct {
		JobIDs []string `json:"job_ids"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		httpErr(w, http.StatusBadRequest, "请求格式错误: "+err.Error())
		return
	}
	id := r.PathValue("id")
	found, empty, deleted := false, false, 0
	s.store.Update(func() bool {
		b := s.store.batchLocked(id)
		if b == nil {
			return false
		}
		found = true
		b.syncIndex()
		var keep []*Job
		var gone []string
		for _, j := range b.Jobs {
			if slices.Contains(req.JobIDs, j.ID) && !j.busy() && !s.runner.IsActive(j.ID) {
				gone = append(gone, j.ID)
				continue
			}
			keep = append(keep, j)
		}
		for _, j := range keep {
			if slices.Contains(gone, j.ParentID) {
				j.ParentID = "" // 来源图片删了，视频变成普通任务继续显示
			}
		}
		b.Jobs, deleted = keep, len(gone)
		empty = len(keep) == 0
		return deleted > 0
	})
	if !found {
		httpErr(w, http.StatusNotFound, "批次不存在")
		return
	}
	if empty {
		_ = s.dropBatch(id)
	}
	writeJSON(w, http.StatusOK, map[string]int{"deleted": deleted, "skipped": len(req.JobIDs) - deleted})
}

// jobRef 返回任务用过的参考图，「重新编辑」用它把图片放回上传区。n 是序号，last 是尾帧。
func (s *Server) jobRef(w http.ResponseWriter, r *http.Request) {
	var src string
	n, err := strconv.Atoi(r.PathValue("n"))
	s.store.View(func() {
		_, j := s.store.jobLocked(r.PathValue("id"))
		switch {
		case j == nil:
		case r.PathValue("n") == "last":
			src = j.LastImage
		case err == nil && n >= 0 && n <= len(j.Refs):
			src = append([]string{j.Image}, j.Refs...)[n]
		}
	})
	if src == "" {
		httpErr(w, http.StatusNotFound, "参考图不存在")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	ref, err := loadRef(ctx, src, true)
	if err != nil || ref == nil {
		httpErr(w, http.StatusNotFound, "参考图读取失败，可能已被移动或删除")
		return
	}
	w.Header().Set("Content-Type", ref.Mime)
	_, _ = w.Write(ref.Data)
}
