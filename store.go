package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"
)

const (
	KindImage = "image"
	KindVideo = "video"

	StatusPending     = "pending"
	StatusSubmitting  = "submitting"
	StatusRunning     = "running"
	StatusDownloading = "downloading"
	StatusSuccess     = "success"
	StatusFailed      = "failed"
	StatusCanceled    = "canceled"
)

type Job struct {
	ID       string `json:"id"`
	Index    int    `json:"index"`
	Kind     string `json:"kind"`
	Prompt   string `json:"prompt"`
	Model    string `json:"model"`
	Size     string `json:"size,omitempty"`
	Duration int    `json:"duration,omitempty"`
	Image    string `json:"image,omitempty"` // 参考图（首尾帧模式下是首帧）：网址或本地路径

	Refs []string `json:"refs,omitempty"` // 第二张起的参考图

	Mode      string `json:"mode,omitempty"`       // 生成模式 references | frames，空 = 自动
	LastImage string `json:"last_image,omitempty"` // 尾帧图片（首尾帧模式）
	Stem      string `json:"stem,omitempty"`       // 输出文件名（不含扩展名），空 = 按序号和提示词命名

	// 图转视频：图片任务记着它的视频提示词和画幅，视频任务记着来源图片。
	VideoPrompt string `json:"video_prompt,omitempty"`
	VideoSize   string `json:"video_size,omitempty"`
	ParentID    string `json:"parent_id,omitempty"`
	VideoSeq    int    `json:"video_seq,omitempty"` // 图片已用过的视频序号，删除后不重复使用

	Note      string   `json:"note,omitempty"`       // 给用户看的说明，例如「已自动补上 mode=references」
	AutoFixes []string `json:"auto_fixes,omitempty"` // 已自动补过的参数名（同一参数不重复补，最多补 3 个）

	Status   string   `json:"status"`
	Progress int      `json:"progress"`
	Error    string   `json:"error,omitempty"`
	Fix      *FixHint `json:"fix,omitempty"`     // 接口报「缺少参数」时的修复建议
	Outputs  []string `json:"outputs,omitempty"` // 相对批次目录的文件名

	// 异步任务提交后记录下来，重启后据此继续查询而不是重新提交（避免重复扣费）。
	RemoteID    string    `json:"remote_id,omitempty"`
	Endpoint    string    `json:"endpoint,omitempty"`
	KeyHash     string    `json:"key_hash,omitempty"`
	Poll        *JobPoll  `json:"poll,omitempty"`
	SubmittedAt time.Time `json:"submitted_at,omitzero"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (j *Job) reset() {
	j.Status = StatusPending
	j.Progress = 0
	j.Error = ""
	j.Fix = nil
	j.Note = ""
	j.AutoFixes = nil
	j.Outputs = nil
	j.RemoteID, j.Endpoint, j.KeyHash = "", "", ""
	j.Poll = nil
	j.SubmittedAt = time.Time{}
	j.UpdatedAt = time.Now()
}

type Batch struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	OutDir    string    `json:"out_dir"`
	CreatedAt time.Time `json:"created_at"`
	Jobs      []*Job    `json:"jobs"`

	Pipeline  *Pipeline `json:"pipeline,omitempty"`   // 「图转视频」批次的视频设置
	LastIndex int       `json:"last_index,omitempty"` // 用过的最大任务序号
}

type Store struct {
	mu      sync.Mutex
	path    string
	batches []*Batch
}

func LoadStore(path string) (*Store, error) {
	s := &Store{path: path, batches: []*Batch{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.batches); err != nil {
		return nil, fmt.Errorf("jobs.json 格式错误: %w", err)
	}
	if s.batches == nil {
		s.batches = []*Batch{}
	}
	return s, nil
}

// Update 在锁内执行 fn，fn 返回 true 时落盘。
func (s *Store) Update(fn func() bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if fn() {
		if err := writeJSONAtomic(s.path, s.batches); err != nil {
			log.Printf("保存任务记录失败: %v", err)
		}
	}
}

func (s *Store) View(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn()
}

func (s *Store) MarshalBatches() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return json.Marshal(s.batches)
}

func (s *Store) batchLocked(id string) *Batch {
	for _, b := range s.batches {
		if b.ID == id {
			return b
		}
	}
	return nil
}

func (s *Store) jobLocked(id string) (*Batch, *Job) {
	for _, b := range s.batches {
		for _, j := range b.Jobs {
			if j.ID == id {
				return b, j
			}
		}
	}
	return nil, nil
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSONAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
