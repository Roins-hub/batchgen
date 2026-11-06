package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"os"
	"path"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

var httpClient = newHTTPClient()

func newHTTPClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = func(req *http.Request) (*url.URL, error) {
		if u := manualProxy.Load(); u != nil {
			return u, nil
		}
		return proxyFunc(req)
	}
	// 只用 HTTP/1.1：HTTP/2 让同时生成的任务共用一条连接，这条连接一断，所有任务的结果一起丢；
	// 慢线路上还会优先传一个任务，其余几乎不动。两处都要设，克隆出来的 TLS 配置里还带着 h2。
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	t.Protocols = protocols
	t.TLSClientConfig = &tls.Config{NextProtos: []string{"http/1.1"}}
	return &http.Client{Transport: t}
}

// manualProxy 是「设置」里填的网络代理，优先于系统代理。
var manualProxy atomic.Pointer[url.URL]

func setManualProxy(raw string) {
	if u, err := url.Parse(raw); raw != "" && err == nil {
		manualProxy.Store(u)
	} else {
		manualProxy.Store(nil)
	}
}

// notSentError 表示请求还没完整发出去就失败了：接口没收到，重试不会重复扣费。
type notSentError struct{ err error }

func (e *notSentError) Error() string { return e.err.Error() }
func (e *notSentError) Unwrap() error { return e.err }

// stallTimeout：响应开始后，这么久没有收到新数据才算断了。
// 不能按总时长算：线路慢时几 MB 的结果要传好几分钟，数据还在流动就不该放弃，否则已扣费的结果会被丢掉。
var stallTimeout = 2 * time.Minute

// stallGuard 返回的 ctx 在 first 时间内没有调用 touch 时取消，取消原因由 cause 给出。
func stallGuard(ctx context.Context, first time.Duration, cause func() error) (context.Context, func(time.Duration), func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	timer := time.AfterFunc(first, func() { cancel(cause()) })
	return ctx, func(d time.Duration) { timer.Reset(d) }, func() { timer.Stop(); cancel(nil) }
}

// countReader 每读到数据就回调一次，用于看门狗和进度。
type countReader struct {
	r  io.Reader
	on func(n int)
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.on(n)
	}
	return n, err
}

func fmtMB(n int64) string { return fmt.Sprintf("%.1f MB", float64(n)/(1<<20)) }

type Client struct {
	base, key, auth string
	headers         map[string]string
}

func NewClient(base, key, auth string, headers map[string]string) *Client {
	return &Client{base: base, key: key, auth: auth, headers: headers}
}

// url 把路径拼到接口地址后面；已经是完整网址的原样返回。
func (c *Client) url(p string) string {
	if isHTTP(p) {
		return p
	}
	return c.base + p
}

// isOwn 判断网址是否属于本接口地址，只有这种情况下载时才带 Key。
func (c *Client) isOwn(u string) bool {
	return u != "" && strings.HasPrefix(u, c.base+"/")
}

func (c *Client) authorize(req *http.Request) {
	switch c.auth {
	case "none":
	case "x-goog-api-key":
		req.Header.Set("x-goog-api-key", c.key)
	case "x-api-key":
		req.Header.Set("x-api-key", c.key)
	case "kling_jwt":
		tok := c.key
		if ak, sk, ok := strings.Cut(c.key, ":"); ok {
			tok = klingJWT(ak, sk)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	default:
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
}

// klingJWT 用可灵官方的 AccessKey/SecretKey 生成 HS256 JWT。
func klingJWT(ak, sk string) string {
	enc := base64.RawURLEncoding
	now := time.Now().Unix()
	head := enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body := enc.EncodeToString([]byte(fmt.Sprintf(`{"iss":%q,"exp":%d,"nbf":%d}`, ak, now+1800, now-5)))
	mac := hmac.New(sha256.New, []byte(sk))
	mac.Write([]byte(head + "." + body))
	return head + "." + body + "." + enc.EncodeToString(mac.Sum(nil))
}

type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	hint := ""
	switch e.Status {
	case 400, 422:
		hint = "（接口不接受这些请求参数）"
	case 401, 403:
		hint = "（API Key 无效或没有权限）"
	case 404:
		hint = "（接口不存在：检查接口地址和接口格式里的路径）"
	case 429:
		hint = "（请求过于频繁或额度不足）"
	}
	msg := innermostMessage(e.Body)
	if strings.Contains(strings.ToLower(msg), "json body required") {
		msg += "（这个接口不接受文件上传，只接受 JSON：请在「设置 → 视频接口」换成用 JSON 传图片的格式，例如「OpenAI 视频 · Seedance 扩展（JSON）」）"
	}
	if strings.Contains(strings.ToLower(msg), "invalid api platform") {
		msg += "（这个模型不走当前接口格式：请在「设置 → 视频接口」换成「OpenAI 视频 · Seedance 扩展」）"
	}
	return fmt.Sprintf("HTTP %d%s：%s", e.Status, hint, msg)
}

// innermostMessage 取出最里层的错误说明。中转站常把上游报错整个塞进 message 字符串里，
// 例如 {"message":"{\"error\":{\"message\":\"mode 为必填项\"}}"}，这里逐层拆开。
func innermostMessage(body string) string {
	msg := strings.TrimSpace(body)
	for range 4 {
		v, err := parseJSON([]byte(msg))
		if err != nil {
			break
		}
		s := strings.TrimSpace(errMsgAny(v))
		if s == "" || s == msg {
			break
		}
		msg = s
	}
	return clip(msg, 300)
}

// retryable 只对明确“没被处理”的错误自动重试：限流、网关不可用、连不上。
// 超时和 500/504 不重试——远端可能已经在生成，重试会重复扣费。
func retryable(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status == 429 || ae.Status == 502 || ae.Status == 503
	}
	var oe *net.OpError
	return errors.As(err, &oe) && oe.Op == "dial"
}

func (c *Client) call(ctx context.Context, method, p string, body []byte, ctype string) ([]byte, error) {
	return c.callWait(ctx, method, p, body, ctype, 0, nil)
}

// callWait 里 wait 是等接口开始响应的时限（0 = 不限，由 ctx 决定）；响应开始后只要还在收到数据就继续等。
// progress 在接收响应时回调，total 未知时是 -1。
func (c *Client) callWait(ctx context.Context, method, p string, body []byte, ctype string, wait time.Duration, progress func(got, total int64)) ([]byte, error) {
	var got atomic.Int64
	var sent atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(i httptrace.WroteRequestInfo) { sent.Store(i.Err == nil) },
	})
	touch := func(time.Duration) {}
	if wait > 0 {
		var stop func()
		ctx, touch, stop = stallGuard(ctx, wait, func() error {
			if n := got.Load(); n > 0 {
				return fmt.Errorf("接收结果时 %d 秒没有新数据（已收到 %s）。结果可能已生成并扣费，请检查网络或代理", int(stallTimeout.Seconds()), fmtMB(n))
			}
			return fmt.Errorf("等了 %d 分钟接口没有响应", int(wait.Minutes()))
		})
		defer stop()
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url(p), rd)
	if err != nil {
		return nil, err
	}
	c.authorize(req)
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		err = friendlyCause(ctx, err)
		if ctx.Err() == nil && !sent.Load() {
			return nil, &notSentError{fmt.Errorf("连不上接口，请检查网络或代理：%w", err)}
		}
		return nil, interrupted(ctx, err, 0)
	}
	defer resp.Body.Close()
	touch(stallTimeout)
	data, err := io.ReadAll(io.LimitReader(&countReader{resp.Body, func(n int) {
		touch(stallTimeout)
		if total := got.Add(int64(n)); progress != nil {
			progress(total, resp.ContentLength)
		}
	}}, 100<<20))
	if err != nil {
		return nil, interrupted(ctx, friendlyCause(ctx, err), got.Load())
	}
	if resp.StatusCode/100 != 2 {
		return nil, &APIError{Status: resp.StatusCode, Body: string(data)}
	}
	return data, nil
}

// friendlyCause 把看门狗取消时的 context canceled 换成具体原因。
func friendlyCause(ctx context.Context, err error) error {
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) && !errors.Is(cause, context.DeadlineExceeded) {
		return cause
	}
	return err
}

// interrupted 说明请求已发出后连接断了：接口可能已经生成并扣费，不能自动重试。
func interrupted(ctx context.Context, err error, got int64) error {
	if ctx.Err() != nil && errors.Is(context.Cause(ctx), context.Canceled) {
		return err // 用户取消
	}
	var ne net.Error
	if !errors.As(err, &ne) && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return err
	}
	return fmt.Errorf("网络连接中断（已收到 %s）。结果可能已生成并扣费，可以到中转站的生图日志查看，或点「重试」重新生成。线路不稳时可以在「设置」里配置网络代理。原始错误：%w", fmtMB(got), err)
}

func (c *Client) ListModels(ctx context.Context, p string) ([]string, error) {
	body, err := c.call(ctx, http.MethodGet, p, nil, "")
	if err != nil {
		return nil, err
	}
	v, err := parseJSON(body)
	if err != nil {
		return nil, err
	}
	models := lookupAll(v, "data.*.id|models.*.name|data.*.name")
	sort.Strings(models)
	return models, nil
}

// ---------- 参考图 ----------

type RefImage struct {
	URL  string // 原始网址（若来自网络）
	Data []byte
	Mime string
}

func (r *RefImage) FileName() string { return "reference" + extForMime(r.Mime, ".png") }

// loadRef 读取参考图。needData=false 且是网址时不下载，直接把网址交给接口。
func loadRef(ctx context.Context, src string, needData bool) (*RefImage, error) {
	src = strings.TrimSpace(src)
	if src == "" {
		return nil, nil
	}
	if isHTTP(src) {
		if !needData {
			return &RefImage{URL: src, Mime: "image/" + strings.TrimPrefix(extFromURL(src, ".png"), ".")}, nil
		}
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
		if err != nil {
			return nil, err
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return nil, fmt.Errorf("下载参考图失败: HTTP %d", resp.StatusCode)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 50<<20))
		if err != nil {
			return nil, err
		}
		return newRef(src, data)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return nil, err
	}
	return newRef("", data)
}

func newRef(u string, data []byte) (*RefImage, error) {
	mime := http.DetectContentType(data)
	if !strings.HasPrefix(mime, "image/") {
		return nil, fmt.Errorf("参考图不是图片文件（识别为 %s）", mime)
	}
	return &RefImage{URL: u, Data: data, Mime: mime}, nil
}

// ---------- 提交与查询 ----------

// Prepared 是渲染好的提交请求。
type Prepared struct {
	Method, Path, ContentType string
	Body                      []byte
	Preview                   string // 给「预览请求」看的请求体
	UsedRef                   bool
}

func prepare(spec *Spec, in GenInput, preview bool) (*Prepared, error) {
	r := spec.Submit
	usedRef := false
	refs := in.allRefs()
	if in.RefLast != nil && in.Ref == nil {
		return nil, errors.New("首尾帧模式需要首帧图片")
	}
	if in.Ref != nil {
		if spec.SubmitRef.Path != "" {
			r = spec.SubmitRef
		} else if !requestUsesImage(r) {
			return nil, errors.New("当前接口格式不支持参考图（请求体里没有用到 {{image}} 等参考图变量）")
		}
		usedRef = true
	}
	vars := buildVars(in, preview)
	p := &Prepared{Method: r.Method, Path: renderPath(r.Path, map[string]string{"model": in.Model}), UsedRef: usedRef}

	v, err := renderTemplate(r.Body, vars)
	if err != nil {
		return nil, err
	}
	if obj, ok := v.(map[string]any); ok {
		applyExtras(obj, in.Extras, r.BodyType != "form", vars)
	}
	if r.BodyType == "form" {
		obj, _ := v.(map[string]any)
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf := &bytes.Buffer{}
		w := multipart.NewWriter(buf)
		var lines []string
		for _, k := range keys {
			val := toFieldString(obj[k])
			if err := w.WriteField(k, val); err != nil {
				return nil, err
			}
			lines = append(lines, k+": "+val)
		}
		type upload struct {
			field, label string
			ref          *RefImage
		}
		field := r.FileField
		if len(refs) > 1 && r.FileMulti && !strings.HasSuffix(field, "[]") {
			field += "[]" // OpenAI 图片接口多张图用 image[]
		}
		var files []upload
		for _, ref := range refs {
			files = append(files, upload{field, "参考图/首帧", ref})
		}
		files = append(files, upload{r.FileFieldLast, "尾帧", in.RefLast})
		for _, f := range files {
			if f.ref == nil || f.field == "" {
				continue
			}
			// 不用 CreateFormFile：它把类型写成 octet-stream，OpenAI 兼容接口会拒收。
			h := make(textproto.MIMEHeader)
			h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, f.field, f.ref.FileName()))
			h.Set("Content-Type", f.ref.Mime)
			part, err := w.CreatePart(h)
			if err != nil {
				return nil, err
			}
			part.Write(f.ref.Data)
			lines = append(lines, f.field+": <"+f.label+"文件 "+f.ref.FileName()+">")
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		p.Body, p.ContentType = buf.Bytes(), w.FormDataContentType()
		p.Preview = "（multipart 表单）\n" + strings.Join(lines, "\n")
		return p, nil
	}
	if r.Body != "" || r.Method != http.MethodGet {
		p.Body, p.ContentType = marshalJSON(v, false), "application/json"
		p.Preview = string(marshalJSON(v, true))
	}
	return p, nil
}

type SubmitResult struct {
	TaskID  string
	Outputs []Output
	Poll    *JobPoll
}

func (c *Client) Submit(ctx context.Context, spec *Spec, in GenInput, wait time.Duration, progress func(got, total int64)) (SubmitResult, error) {
	p, err := prepare(spec, in, false)
	if err != nil {
		return SubmitResult{}, err
	}
	data, err := c.callWait(ctx, p.Method, p.Path, p.Body, p.ContentType, wait, progress)
	if err != nil {
		return SubmitResult{}, err
	}
	v, err := parseJSON(data)
	if err != nil {
		return SubmitResult{}, err
	}

	if spec.TaskID == "" { // 同步接口
		outs := extractOutputs(v, spec.Result, in.Kind)
		if len(outs) == 0 {
			return SubmitResult{}, fmt.Errorf("响应里没有找到结果文件: %s", firstNonEmpty(errMsgAny(v), clip(string(data), 300)))
		}
		return SubmitResult{Outputs: c.markAuth(outs)}, nil
	}

	id := lookupFirst(v, spec.TaskID)
	if id == "" {
		return SubmitResult{}, fmt.Errorf("提交后没有拿到任务 ID（字段 %s）: %s", spec.TaskID, firstNonEmpty(errMsgAny(v), clip(string(data), 300)))
	}
	if matchState(lookupFirst(v, spec.Poll.Status), spec.Poll.Success, spec.Poll.Fail) == stateFailed {
		return SubmitResult{}, errors.New(firstNonEmpty(lookupFirst(v, spec.Poll.Error), errMsgAny(v), "远端拒绝了任务"))
	}
	qp := spec.Poll.Path
	if p.UsedRef && spec.Poll.PathRef != "" {
		qp = spec.Poll.PathRef
	}
	model := map[string]string{"model": in.Model}
	return SubmitResult{TaskID: id, Poll: &JobPoll{
		Path: renderPath(qp, model), Status: spec.Poll.Status, Success: spec.Poll.Success, Fail: spec.Poll.Fail,
		Progress: spec.Poll.Progress, Error: spec.Poll.Error, FileID: spec.Poll.FileID,
		Content: renderPath(spec.Poll.Content, model), Result: spec.Result,
		Auth: spec.Auth, Headers: spec.Headers,
	}}, nil
}

type PollStatus struct {
	State    string
	Progress int
	Outputs  []Output
	FileID   string
	Error    string
	Raw      string
}

func (c *Client) Query(ctx context.Context, jp *JobPoll, id, kind string) (PollStatus, error) {
	data, err := c.call(ctx, http.MethodGet, renderPath(jp.Path, map[string]string{"id": id}), nil, "")
	if err != nil {
		return PollStatus{}, err
	}
	v, err := parseJSON(data)
	if err != nil {
		return PollStatus{}, err
	}
	st := PollStatus{Raw: clip(string(data), 300), Progress: parseProgress(lookupFirst(v, jp.Progress))}
	if jp.Status == "" {
		// 没有状态字段：拿到结果就算完成
		if st.Outputs = extractOutputs(v, jp.Result, kind); len(st.Outputs) > 0 {
			st.State = stateSuccess
		} else {
			st.State = stateRunning
		}
	} else {
		st.State = matchState(lookupFirst(v, jp.Status), jp.Success, jp.Fail)
		if st.State == stateSuccess {
			st.Outputs = extractOutputs(v, jp.Result, kind)
		}
	}
	switch st.State {
	case stateSuccess:
		st.FileID = lookupFirst(v, jp.FileID)
		st.Error = lookupFirst(v, jp.Error)
		st.Outputs = c.markAuth(st.Outputs)
	case stateFailed:
		st.Error = firstNonEmpty(lookupFirst(v, jp.Error), errMsgAny(v))
	}
	return st, nil
}

func (c *Client) markAuth(outs []Output) []Output {
	for i := range outs {
		outs[i].Auth = c.isOwn(outs[i].URL)
	}
	return outs
}

// ---------- 保存结果 ----------

func (c *Client) SaveOutput(ctx context.Context, o Output, destNoExt, kind string) (string, error) {
	fallback := ".png"
	if kind == KindVideo {
		fallback = ".mp4"
	}
	if o.B64 == "" {
		return c.download(ctx, o.URL, destNoExt, fallback, kind, o.Auth, 0)
	}
	raw := o.B64
	if i := strings.Index(raw, ","); strings.HasPrefix(raw, "data:") && i > 0 {
		raw = raw[i+1:]
	}
	raw = strings.NewReplacer("\r", "", "\n", "").Replace(raw)
	data, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return "", fmt.Errorf("base64 解码失败: %w", err)
	}
	dest := destNoExt + extForMime(http.DetectContentType(data), fallback)
	return dest, os.WriteFile(dest, data, 0o644)
}

// download 保存文件。如果下载接口返回的是 JSON（如 MiniMax 的 files/retrieve），从里面再找文件地址。
func (c *Client) download(ctx context.Context, rawURL, destNoExt, fallbackExt, kind string, withAuth bool, depth int) (string, error) {
	parent := ctx
	ctx, touch, stop := stallGuard(ctx, stallTimeout, func() error {
		return fmt.Errorf("下载时 %d 秒没有新数据，请检查网络或代理", int(stallTimeout.Seconds()))
	})
	defer stop()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	if withAuth {
		c.authorize(req)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", friendlyCause(ctx, err)
	}
	defer resp.Body.Close()
	touch(stallTimeout)
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", &APIError{Status: resp.StatusCode, Body: string(b)}
	}

	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "json") && depth == 0 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		v, err := parseJSON(data)
		if err != nil {
			return "", err
		}
		outs := extractOutputs(v, "", kind)
		if len(outs) == 0 {
			return "", fmt.Errorf("下载接口返回了 JSON，但里面没有文件地址: %s", clip(string(data), 300))
		}
		if outs[0].B64 != "" {
			return c.SaveOutput(parent, outs[0], destNoExt, kind)
		}
		return c.download(parent, outs[0].URL, destNoExt, fallbackExt, kind, c.isOwn(outs[0].URL), depth+1)
	}

	ext := firstNonEmpty(extForMime(ct, ""), extFromURL(rawURL, ""), fallbackExt)
	dest := destNoExt + ext
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(f, &countReader{resp.Body, func(int) { touch(stallTimeout) }})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return "", friendlyCause(ctx, err)
	}
	return dest, os.Rename(tmp, dest)
}

// ---------- 小工具 ----------

func isHTTP(s string) bool {
	s = strings.ToLower(s)
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

func extFromURL(raw, fallback string) string {
	if u, err := url.Parse(raw); err == nil {
		ext := strings.ToLower(path.Ext(u.Path))
		if slices.Contains([]string{".png", ".jpg", ".jpeg", ".webp", ".gif", ".mp4", ".webm", ".mov", ".m4v"}, ext) {
			return ext
		}
	}
	return fallback
}

func extForMime(ct, fallback string) string {
	ct = strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
	switch ct {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "video/mp4":
		return ".mp4"
	case "video/webm":
		return ".webm"
	case "video/quicktime":
		return ".mov"
	}
	return fallback
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
