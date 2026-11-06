package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

// Runner 负责调度：按类型限制并发，提交、轮询、下载，并把每一步状态写回 Store。
type Runner struct {
	store *Store
	cfg   *ConfigStore

	mu     sync.Mutex // 加锁顺序：store.mu → r.mu
	active map[string]context.CancelFunc
	counts map[string]int

	keyIdx   atomic.Uint64
	uploader *Uploader
	wake     chan struct{}
}

func NewRunner(store *Store, cfg *ConfigStore) *Runner {
	return &Runner{
		store:    store,
		cfg:      cfg,
		active:   map[string]context.CancelFunc{},
		counts:   map[string]int{},
		uploader: NewUploader(),
		wake:     make(chan struct{}, 1),
	}
}

// Recover 处理上次退出时没跑完的任务：已拿到远端任务 ID 的继续查询，其余无法确认的标记失败。
func (r *Runner) Recover() {
	const lost = "上次运行时程序在生成过程中被关闭，无法确认远端是否已执行（可能已计费）。如需重新生成请点「重试」。"
	r.store.Update(func() bool {
		changed := false
		for _, b := range r.store.batches {
			for _, j := range b.Jobs {
				switch j.Status {
				case StatusSubmitting, StatusRunning, StatusDownloading:
					if j.RemoteID != "" && j.Poll != nil {
						j.Status = StatusRunning
					} else {
						j.Status = StatusFailed
						j.Error = lost
					}
					changed = true
				}
			}
		}
		return changed
	})
}

func (r *Runner) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *Runner) Loop() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-r.wake:
		}
		r.schedule()
	}
}

func (r *Runner) schedule() {
	cfg := r.cfg.Get()
	limit := map[string]int{KindImage: cfg.ImageConcurrency, KindVideo: cfg.VideoConcurrency}
	ready := map[string]bool{}
	for _, k := range []string{KindImage, KindVideo} {
		base, keys := cfg.endpoint(k)
		ready[k] = base != "" && len(keys) > 0
	}
	r.store.Update(func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		changed := false
		for _, b := range r.store.batches {
			for _, j := range b.Jobs {
				resume := j.Status == StatusRunning && j.RemoteID != ""
				if (j.Status != StatusPending && !resume) || (!resume && !ready[j.Kind]) {
					continue
				}
				if _, busy := r.active[j.ID]; busy || r.counts[j.Kind] >= limit[j.Kind] {
					continue
				}
				ctx, cancel := context.WithCancel(context.Background())
				r.active[j.ID] = cancel
				r.counts[j.Kind]++
				if j.Status == StatusPending {
					j.Status = StatusSubmitting
					j.Error = ""
					j.UpdatedAt = time.Now()
					changed = true
				}
				go r.run(ctx, j.ID, j.Kind)
			}
		}
		return changed
	})
}

func (r *Runner) ActiveCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.active)
}

// Idle 在没有任务运行或排队时返回 true。
func (r *Runner) Idle() bool {
	idle := r.ActiveCount() == 0
	r.store.View(func() {
		for _, b := range r.store.batches {
			for _, j := range b.Jobs {
				switch j.Status {
				case StatusPending, StatusSubmitting, StatusRunning, StatusDownloading:
					idle = false
				}
			}
		}
	})
	return idle
}

func (r *Runner) IsActive(jobID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.active[jobID]
	return ok
}

// Cancel 取消匹配的任务：运行中的打断，排队中的直接标记取消。
func (r *Runner) Cancel(match func(b *Batch, j *Job) bool) int {
	var cancels []context.CancelFunc
	n := 0
	r.store.Update(func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		changed := false
		for _, b := range r.store.batches {
			for _, j := range b.Jobs {
				if !match(b, j) {
					continue
				}
				if c, ok := r.active[j.ID]; ok {
					cancels = append(cancels, c)
					n++
					continue
				}
				if j.Status == StatusPending || j.Status == StatusRunning {
					j.Status = StatusCanceled
					j.Error = "已取消"
					j.UpdatedAt = time.Now()
					changed = true
					n++
				}
			}
		}
		return changed
	})
	for _, c := range cancels {
		c()
	}
	return n
}

func (r *Runner) update(jobID string, fn func(j *Job)) {
	r.store.Update(func() bool {
		_, j := r.store.jobLocked(jobID)
		if j == nil {
			return false
		}
		fn(j)
		j.UpdatedAt = time.Now()
		return true
	})
}

func (r *Runner) run(ctx context.Context, jobID, kind string) {
	defer func() {
		r.mu.Lock()
		delete(r.active, jobID)
		r.counts[kind]--
		r.mu.Unlock()
		r.Wake()
	}()

	var job Job
	var outDir string
	batchSize := 0
	found := false
	r.store.View(func() {
		if b, j := r.store.jobLocked(jobID); j != nil {
			job, outDir, found = *j, b.OutDir, true
			batchSize = len(b.Jobs)
		}
	})
	if !found {
		return
	}

	cfg := r.cfg.Get()
	if err := r.runJob(ctx, cfg, job, outDir); err != nil {
		canceled := ctx.Err() != nil
		var fe *fixableError
		errors.As(err, &fe)

		// 接口只收图片网址、不收 base64：把这个接口改成「先上传图床」并重试（只做一次）。
		if fe != nil && !canceled && !cfg.ManualFix && cfg.ImageHost.Mode != "off" &&
			!slices.Contains(job.AutoFixes, "image_delivery") && hasLocalImage(job) && needsImageURL(fe.err) {
			r.setImageDelivery(job.Kind, "upload")
			fixes := append(slices.Clone(job.AutoFixes), "image_delivery")
			note := "接口只接受图片网址，已改为先上传图床再发送"
			if job.Note != "" {
				note = job.Note + "；" + note
			}
			r.update(jobID, func(j *Job) {
				j.reset()
				j.AutoFixes, j.Note = fixes, note
			})
			return
		}

		// 接口说缺参数：自动补上、记住，再重试。中转站常一个一个地报缺参数，
		// 所以每个任务可补的参数个数跟随批次任务数（至少 10 个）；同一个参数补过还报错就交给用户，避免死循环。
		if fe != nil && fe.hint != nil && !canceled && !cfg.ManualFix &&
			len(job.AutoFixes) < autoFixLimit(batchSize) && !slices.Contains(job.AutoFixes, fe.hint.Param) {
			if val := chooseFixValue(job, fe.hint); val != "" {
				r.learnParam(job.Kind, job.Model, fe.hint.Param, val)
				fixes := append(slices.Clone(job.AutoFixes), fe.hint.Param)
				part := fe.hint.Param + "=" + displayValue(val)
				note := "接口要求的参数已自动补上并重试：" + part
				if job.Note != "" {
					note = job.Note + "、" + part
				}
				r.update(jobID, func(j *Job) {
					j.reset()
					j.AutoFixes, j.Note = fixes, note
				})
				return
			}
		}

		r.update(jobID, func(j *Job) {
			if fe != nil {
				j.Fix = fe.hint
			}
			if canceled {
				j.Status = StatusCanceled
				j.Error = "已取消"
				if j.RemoteID != "" {
					j.Error = "已取消（远端任务可能仍在生成并计费）"
				}
			} else {
				j.Status = StatusFailed
				j.Error = err.Error()
			}
		})
	}
}

// autoMode：「自动」模式下有尾帧用首尾帧，否则用全能参考（文生视频、参考图都走这个）。
func autoMode(j Job) string {
	if j.LastImage != "" {
		return "frames"
	}
	return "references"
}

// withJobMode 处理生成模式：明确选了就带上 mode；选「自动」且这个模型已知需要 mode 时，按有没有尾帧决定取值。
func withJobMode(extras []ExtraParam, j Job) []ExtraParam {
	want := j.Mode
	found := false
	out := slices.Clone(extras)
	for i := range out {
		if out[i].Key != "mode" {
			continue
		}
		found = true
		if want == "" && (out[i].Value == "references" || out[i].Value == "frames") {
			out[i].Value = autoMode(j)
		} else if want != "" {
			out[i].Value = want
		}
	}
	if !found && want != "" {
		out = append(out, ExtraParam{Kind: j.Kind, Key: "mode", Value: want})
	}
	return out
}

// autoFixLimit：每个任务最多自动补几个参数，跟随批次任务数，最少 10 个。
// 真正防止反复提交的是「同一个参数不重复补」，这个上限只是兜底。
func autoFixLimit(batchSize int) int {
	return max(10, batchSize)
}

// idParamRe 认出「每个任务一个唯一值」的参数，例如 client_task_id、request_id、trace_id、nonce。
var idParamRe = regexp.MustCompile(`(?i)(^|[_.-])(task_?id|request_?id|req_?id|trace_?id|client_?id|uuid|guid|nonce|idempotency(_?key)?|external_task_id|biz_?id)$|^id$`)

// displayValue 把值里的变量换成人能看懂的说明，用在卡片提示里。
func displayValue(v string) string {
	switch v {
	case "{{uuid}}":
		return "自动生成的唯一 ID"
	case "{{job_id}}":
		return "任务编号"
	case "{{duration}}":
		return "任务时长"
	case "{{size}}", "{{aspect_ratio}}", "{{resolution}}":
		return "按任务尺寸"
	case "{{prompt}}":
		return "提示词"
	case "{{model}}":
		return "模型名"
	}
	return v
}

// semanticParams：接口要的其实是任务本身已有的设置，用模板变量填，每个任务取自己的值。
// 例如接口只认 duration 而请求里发的是 seconds，就补上 duration={{duration}}（按数字发送）。
var semanticParams = []struct {
	re  *regexp.Regexp
	val string
	has func(Job) bool
}{
	{regexp.MustCompile(`(?i)^(duration|seconds|video_?duration|duration_?seconds|length)$`), "{{duration}}", func(j Job) bool { return j.Duration > 0 }},
	{regexp.MustCompile(`(?i)^(aspect_?ratio|ratio)$`), "{{aspect_ratio}}", func(j Job) bool { _, _, a, _ := parseSize(j.Size); return a != "" }},
	{regexp.MustCompile(`(?i)^resolution$`), "{{resolution}}", func(j Job) bool { _, _, _, r := parseSize(j.Size); return r != "" }},
	{regexp.MustCompile(`(?i)^size$`), "{{size}}", func(j Job) bool { return j.Size != "" }},
	{regexp.MustCompile(`(?i)^(prompt|text)$`), "{{prompt}}", func(j Job) bool { return j.Prompt != "" }},
	{regexp.MustCompile(`(?i)^(model|model_?name)$`), "{{model}}", func(j Job) bool { return j.Model != "" }},
}

// chooseFixValue 为接口要求的参数挑一个值；挑不出来返回空（交给用户在卡片上选）。
func chooseFixValue(j Job, h *FixHint) string {
	if idParamRe.MatchString(h.Param) && len(h.Options) == 0 {
		return "{{uuid}}" // 每次提交现生成，不会和别的任务重复
	}
	for _, sp := range semanticParams {
		if sp.re.MatchString(h.Param) && len(h.Options) == 0 && sp.has(j) {
			return sp.val
		}
	}
	if strings.EqualFold(h.Param, "mode") {
		want := firstNonEmpty(j.Mode, autoMode(j))
		if len(h.Options) == 0 || slices.Contains(h.Options, want) {
			return want
		}
	}
	if len(h.Options) > 0 {
		return h.Options[0]
	}
	return ""
}

func hasLocalImage(j Job) bool {
	for _, s := range append([]string{j.Image, j.LastImage}, j.Refs...) {
		if s != "" && !isHTTP(s) {
			return true
		}
	}
	return false
}

// setImageDelivery 修改某类接口的参考图发送方式并保存。
func (r *Runner) setImageDelivery(kind, mode string) {
	c := r.cfg.Get()
	if kind == KindVideo {
		c.VideoSpec.ImageDelivery = mode
	} else {
		c.ImageSpec.ImageDelivery = mode
	}
	c.normalize()
	if err := r.cfg.Set(c); err != nil {
		log.Printf("保存参考图发送方式失败: %v", err)
	}
}

// learnParam 把参数记成附加参数规则（只作用于这个模型）。
func (r *Runner) learnParam(kind, model, key, value string) {
	c := r.cfg.Get()
	c.ExtraParams = append(c.ExtraParams, ExtraParam{Kind: kind, Model: model, Key: key, Value: value})
	c.normalize()
	if err := r.cfg.Set(c); err != nil {
		log.Printf("保存附加参数失败: %v", err)
	}
}

// fixableError 是提交阶段的失败，附带「缺少参数」的修复建议（可能为 nil）。
type fixableError struct {
	err  error
	hint *FixHint
}

func (e *fixableError) Error() string { return e.err.Error() }
func (e *fixableError) Unwrap() error { return e.err }

// runJob：提交 →（同步接口直接拿结果 / 异步接口轮询）→ 下载保存。
func (r *Runner) runJob(ctx context.Context, cfg Config, job Job, outDir string) error {
	var c *Client
	var outs []Output

	if job.RemoteID == "" {
		spec := cfg.Spec(job.Kind)
		base, keys := cfg.endpoint(job.Kind)
		var refs []*RefImage
		for _, src := range append([]string{job.Image}, job.Refs...) {
			ref, err := loadRef(ctx, src, spec.needsRefData())
			if err != nil {
				return fmt.Errorf("读取参考图失败: %w", err)
			}
			if ref != nil {
				refs = append(refs, ref)
			}
		}
		var ref *RefImage
		if len(refs) > 0 {
			ref = refs[0]
		}
		refLast, err := loadRef(ctx, job.LastImage, spec.needsRefData())
		if err != nil {
			return fmt.Errorf("读取尾帧失败: %w", err)
		}
		// 接口只收图片网址：本地图片先传到图床（同一张图会复用网址）
		if spec.ImageDelivery == "upload" {
			for _, rf := range append(slices.Clone(refs), refLast) {
				if rf == nil || rf.URL != "" {
					continue
				}
				link, err := r.uploader.Upload(ctx, cfg.ImageHost, rf)
				if err != nil {
					return fmt.Errorf("上传参考图失败：%w", err)
				}
				rf.URL = link
			}
		}
		key := r.pickKey(keys, "")
		c = NewClient(base, key, spec.Auth, spec.Headers)
		in := GenInput{JobID: job.ID, Kind: job.Kind, Model: job.Model, Prompt: job.Prompt, Size: job.Size, Duration: job.Duration,
			Mode: firstNonEmpty(job.Mode, autoMode(job)),
			Ref:  ref, Refs: refs, RefLast: refLast, Extras: withJobMode(extrasFor(cfg.ExtraParams, job.Kind, job.Model), job)}
		timeout := 3 * time.Minute
		if spec.TaskID == "" {
			timeout = 10 * time.Minute // 同步接口要等生成完才返回
		}
		// 同步接口的结果在响应里，线路慢时要传好几分钟：把接收进度显示在卡片上。
		var shown time.Time
		progress := func(got, total int64) {
			if spec.TaskID != "" || time.Since(shown) < 2*time.Second {
				return
			}
			shown = time.Now()
			r.update(job.ID, func(j *Job) {
				j.Status = StatusDownloading
				j.Progress = 0
				if total > 0 {
					j.Progress = int(got * 100 / total)
				}
			})
		}
		res, err := withRetry(ctx, cfg.MaxRetries, func() (SubmitResult, error) {
			return c.Submit(ctx, &spec, in, timeout, progress)
		})
		if err != nil {
			if ctx.Err() != nil {
				return err
			}
			return &fixableError{err: fmt.Errorf("提交失败，%w", err), hint: fixHintFrom(err)}
		}
		if res.TaskID == "" {
			outs = res.Outputs
		} else {
			job.RemoteID, job.Poll, job.Endpoint, job.SubmittedAt = res.TaskID, res.Poll, base, time.Now()
			r.update(job.ID, func(j *Job) {
				j.Status = StatusRunning
				j.RemoteID, j.Poll, j.Endpoint, j.KeyHash = res.TaskID, res.Poll, base, keyHash(key)
				j.SubmittedAt = job.SubmittedAt
			})
		}
	} else {
		if job.Poll == nil {
			return errors.New("这条任务记录来自旧版本，无法继续查询，请点「重试」")
		}
		// 续跑：用提交时的同一个 Key 和地址查询，否则可能查不到任务。
		keys := append(slices.Clone(cfg.Spec(job.Kind).APIKeys), cfg.APIKeys...)
		c = NewClient(job.Endpoint, r.pickKey(keys, job.KeyHash), job.Poll.Auth, job.Poll.Headers)
	}

	if job.RemoteID != "" {
		var err error
		if outs, err = r.poll(ctx, cfg, c, job); err != nil {
			return err
		}
	}

	r.update(job.ID, func(j *Job) { j.Status = StatusDownloading; j.Progress = 100 })
	base := filepath.Join(outDir, fileBase(job))
	var files []string
	for i, o := range outs {
		name := base
		if len(outs) > 1 {
			name = fmt.Sprintf("%s_%d", base, i+1)
		}
		p, err := c.SaveOutput(ctx, o, name, job.Kind)
		if err != nil {
			return fmt.Errorf("保存结果失败: %w", err)
		}
		files = append(files, filepath.Base(p))
	}
	writeMeta(base+".json", job)
	r.store.Update(func() bool {
		b, j := r.store.jobLocked(job.ID)
		if j == nil {
			return false
		}
		j.Status, j.Outputs, j.Error, j.UpdatedAt = StatusSuccess, files, "", time.Now()
		if b.Pipeline != nil && b.Pipeline.Auto && j.Kind == KindImage && b.videoCount(j.ID) == 0 {
			b.addVideos(j, *b.Pipeline)
		}
		return true
	})
	return nil
}

func (r *Runner) poll(ctx context.Context, cfg Config, c *Client, job Job) ([]Output, error) {
	deadline := job.SubmittedAt.Add(time.Duration(cfg.VideoTimeoutMin) * time.Minute)
	interval := time.Duration(cfg.PollIntervalSec) * time.Second
	failures, lastProgress := 0, -1
	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("等待超时（%d 分钟），远端任务 ID：%s", cfg.VideoTimeoutMin, job.RemoteID)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
		cctx, cancel := context.WithTimeout(ctx, time.Minute)
		st, err := c.Query(cctx, job.Poll, job.RemoteID, job.Kind)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if failures++; failures >= 10 {
				return nil, fmt.Errorf("查询任务状态连续失败: %w", err)
			}
			continue
		}
		failures = 0
		switch st.State {
		case stateFailed:
			return nil, errors.New(firstNonEmpty(st.Error, "远端任务失败: "+st.Raw))
		case stateSuccess:
			if len(st.Outputs) > 0 {
				return st.Outputs, nil
			}
			if tpl := job.Poll.Content; tpl != "" {
				if strings.Contains(tpl, "{file_id}") && st.FileID == "" {
					return nil, fmt.Errorf("任务已完成，但没拿到文件 ID（字段 %s）: %s", job.Poll.FileID, st.Raw)
				}
				u := c.url(renderPath(tpl, map[string]string{"id": job.RemoteID, "file_id": st.FileID}))
				return []Output{{URL: u, Auth: true}}, nil
			}
			if st.Error != "" {
				return nil, errors.New(st.Error)
			}
			return nil, fmt.Errorf("任务已完成，但响应里找不到结果文件（检查接口格式里的「结果字段」）: %s", st.Raw)
		}
		if st.Progress != lastProgress {
			lastProgress = st.Progress
			r.update(job.ID, func(j *Job) { j.Progress = st.Progress })
		}
	}
}

// pickKey 优先返回与 hash 匹配的 Key（续跑用），否则轮流使用。
func (r *Runner) pickKey(keys []string, hash string) string {
	if hash != "" {
		for _, k := range keys {
			if keyHash(k) == hash {
				return k
			}
		}
	}
	if len(keys) == 0 {
		return ""
	}
	i := r.keyIdx.Add(1) - 1
	return keys[i%uint64(len(keys))]
}

func keyHash(k string) string {
	sum := sha256.Sum256([]byte(k))
	return hex.EncodeToString(sum[:8])
}

// connectRetries 是请求没发出去时的重试次数。线路间歇性不通时常常持续一两分钟，而这种重试不花钱，可以多等一会儿。
var (
	connectRetries = 8
	connectDelay   = 15 * time.Second
)

func withRetry[T any](ctx context.Context, maxRetries int, fn func() (T, error)) (T, error) {
	var zero T
	attempt, unsent := 0, 0
	for {
		v, err := fn()
		if err == nil {
			return v, nil
		}
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
		var ns *notSentError
		wait := connectDelay
		if errors.As(err, &ns) {
			if unsent++; unsent > connectRetries {
				return zero, err
			}
		} else {
			if !retryable(err) || attempt >= maxRetries {
				return zero, err
			}
			wait = time.Duration(2<<attempt) * time.Second
			attempt++
		}
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-time.After(wait):
		}
	}
}

func fileBase(j Job) string {
	if j.Stem != "" {
		return j.Stem
	}
	return fmt.Sprintf("%03d_%s", j.Index, sanitize(j.Prompt, 24))
}

// sanitize 把文本变成安全的文件名片段。
func sanitize(s string, maxRunes int) string {
	var b strings.Builder
	n := 0
	for _, c := range s {
		if n >= maxRunes {
			break
		}
		if strings.ContainsRune(`\/:*?"<>|`, c) || unicode.IsControl(c) || unicode.IsSpace(c) {
			c = '_'
		}
		b.WriteRune(c)
		n++
	}
	out := strings.Trim(b.String(), "_. ")
	if out == "" {
		return "untitled"
	}
	return out
}

func writeMeta(path string, j Job) {
	_ = writeJSONAtomic(path, map[string]any{
		"kind":       j.Kind,
		"prompt":     j.Prompt,
		"model":      j.Model,
		"size":       j.Size,
		"duration":   j.Duration,
		"reference":  j.Image,
		"references": j.Refs,
		"remote_id":  j.RemoteID,
		"created_at": time.Now().Format(time.RFC3339),
	})
}
