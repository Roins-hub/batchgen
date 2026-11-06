package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ImageHost 是参考图的「图床」设置：有些接口只收图片网址，本地图片要先上传拿到网址。
type ImageHost struct {
	Mode        string `json:"mode"`   // auto（免费临时图床，自动切换）| s3 | off
	Expiry      string `json:"expiry"` // litterbox 保留时长：1h | 12h | 24h | 72h
	Endpoint    string `json:"endpoint"`
	Region      string `json:"region"`
	Bucket      string `json:"bucket"`
	AccessKey   string `json:"access_key"`
	SecretKey   string `json:"secret_key"`
	PublicBase  string `json:"public_base"` // 公开访问地址前缀，例如 https://img.example.com
	Prefix      string `json:"prefix"`      // 对象路径前缀
	VirtualHost bool   `json:"virtual_host"`
}

func (h *ImageHost) normalize() {
	switch h.Mode {
	case "s3", "off":
	default:
		// 旧配置的 litterbox 基本都是默认值，不是用户选的，一起改成自动切换
		h.Mode = "auto"
	}
	switch h.Expiry {
	case "1h", "12h", "24h", "72h":
	default:
		h.Expiry = "24h"
	}
	h.Endpoint = strings.TrimRight(strings.TrimSpace(h.Endpoint), "/")
	if h.Endpoint != "" && !strings.Contains(h.Endpoint, "://") {
		h.Endpoint = "https://" + h.Endpoint
	}
	h.Region = firstNonEmpty(strings.TrimSpace(h.Region), "auto")
	h.Bucket = strings.TrimSpace(h.Bucket)
	h.AccessKey = strings.TrimSpace(h.AccessKey)
	h.SecretKey = strings.TrimSpace(h.SecretKey)
	h.PublicBase = strings.TrimRight(strings.TrimSpace(h.PublicBase), "/")
	h.Prefix = strings.Trim(strings.TrimSpace(h.Prefix), "/")
	if h.Prefix == "" {
		h.Prefix = "batchgen"
	}
}

// Uploader 上传参考图并缓存网址：同一张图在有效期内只传一次（一批几十个任务共用一张参考图时很重要）。
type Uploader struct {
	mu    sync.Mutex
	cache map[string]cachedURL
	good  int // 上次成功的临时图床，之后的任务先试它，不用再等不通的那家超时
}

type cachedURL struct {
	url     string
	expires time.Time
}

func NewUploader() *Uploader { return &Uploader{cache: map[string]cachedURL{}} }

func (u *Uploader) Upload(ctx context.Context, h ImageHost, ref *RefImage) (string, error) {
	if h.Mode == "off" {
		return "", errors.New("这个接口只接受图片网址，但「设置 → 参考图上传」已关闭；请开启图床，或在 CSV 里直接填图片网址")
	}
	sum := sha256.Sum256(ref.Data)
	digest := hex.EncodeToString(sum[:])
	cacheKey := h.Mode + "|" + h.Endpoint + "|" + h.Bucket + "|" + digest

	u.mu.Lock()
	c, ok := u.cache[cacheKey]
	u.mu.Unlock()
	if ok && time.Now().Before(c.expires) {
		return c.url, nil
	}

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var link string
	var ttl time.Duration
	var err error
	if h.Mode == "s3" {
		link, err = uploadS3(ctx, h, ref, digest)
		ttl = 7 * 24 * time.Hour
	} else {
		link, ttl, err = u.uploadTemp(ctx, h, ref)
	}
	if err != nil {
		return "", err
	}
	u.mu.Lock()
	u.cache[cacheKey] = cachedURL{url: link, expires: time.Now().Add(ttl)}
	u.mu.Unlock()
	return link, nil
}

// 写成变量是为了测试时换成本地假服务。
var (
	uguuAPI      = "https://uguu.se/upload"
	litterboxAPI = "https://litterbox.catbox.moe/resources/internals/api.php"
)

// tempHostTimeout 是单家的上限：两家都试完不能超过 Upload 的 2 分钟。
const tempHostTimeout = 55 * time.Second

// tempHost 是一家免费临时图床。ttl 要比实际保留时长短，快过期的链接不再复用。
// 新增时先确认中转站能下载到真实图片：tmpfiles.org 的链接下载到的是网页，不能用。
type tempHost struct {
	upload func(ctx context.Context, h ImageHost, ref *RefImage) (string, error)
	ttl    func(h ImageHost) time.Duration
}

var tempHosts = []tempHost{
	{
		upload: func(ctx context.Context, _ ImageHost, ref *RefImage) (string, error) { return uploadUguu(ctx, ref) },
		ttl:    func(ImageHost) time.Duration { return 2 * time.Hour }, // 官方保留 3 小时
	},
	{
		upload: func(ctx context.Context, h ImageHost, ref *RefImage) (string, error) {
			return uploadLitterbox(ctx, h.Expiry, ref)
		},
		ttl: func(h ImageHost) time.Duration {
			d, _ := time.ParseDuration(h.Expiry)
			return max(d-time.Hour, 30*time.Minute)
		},
	},
}

// uploadTemp 依次尝试各家临时图床，从上次成功的那家开始。
func (u *Uploader) uploadTemp(ctx context.Context, h ImageHost, ref *RefImage) (string, time.Duration, error) {
	u.mu.Lock()
	first := u.good
	u.mu.Unlock()
	var fails []string
	for i := range tempHosts {
		index := (first + i) % len(tempHosts)
		attempt, cancel := context.WithTimeout(ctx, tempHostTimeout)
		link, err := tempHosts[index].upload(attempt, h, ref)
		cancel()
		if err == nil {
			u.mu.Lock()
			u.good = index
			u.mu.Unlock()
			return link, tempHosts[index].ttl(h), nil
		}
		if ctx.Err() != nil {
			return "", 0, ctx.Err()
		}
		fails = append(fails, err.Error())
	}
	return "", 0, errors.New(strings.Join(fails, "；"))
}

func uploadUguu(ctx context.Context, ref *RefImage) (string, error) {
	buf := &bytes.Buffer{}
	w := multipart.NewWriter(buf)
	part, err := w.CreateFormFile("files[]", ref.FileName())
	if err != nil {
		return "", err
	}
	part.Write(ref.Data)
	w.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uguuAPI, buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("上传到 uguu 失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	var out struct {
		Success bool `json:"success"`
		Files   []struct {
			URL string `json:"url"`
		} `json:"files"`
	}
	if err := json.Unmarshal(body, &out); err != nil || resp.StatusCode/100 != 2 || !out.Success || len(out.Files) == 0 || !isHTTP(out.Files[0].URL) {
		return "", fmt.Errorf("上传到 uguu 失败: HTTP %d %s", resp.StatusCode, clip(string(body), 200))
	}
	return out.Files[0].URL, nil
}

func uploadLitterbox(ctx context.Context, expiry string, ref *RefImage) (string, error) {
	buf := &bytes.Buffer{}
	w := multipart.NewWriter(buf)
	w.WriteField("reqtype", "fileupload")
	w.WriteField("time", expiry)
	part, err := w.CreateFormFile("fileToUpload", ref.FileName())
	if err != nil {
		return "", err
	}
	part.Write(ref.Data)
	w.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, litterboxAPI, buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("上传到 litterbox 失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	link := strings.TrimSpace(string(body))
	if resp.StatusCode/100 != 2 || !isHTTP(link) {
		return "", fmt.Errorf("上传到 litterbox 失败: HTTP %d %s", resp.StatusCode, clip(link, 200))
	}
	return link, nil
}

// uploadS3 用 AWS Signature V4 上传到 S3 兼容存储（R2、OSS、COS、MinIO 等）。
func uploadS3(ctx context.Context, h ImageHost, ref *RefImage, digest string) (string, error) {
	if h.Endpoint == "" || h.Bucket == "" || h.AccessKey == "" || h.SecretKey == "" || h.PublicBase == "" {
		return "", errors.New("自己的存储没有配置完整：需要接口地址、Bucket、Access Key、Secret Key 和公开访问地址")
	}
	ep, err := url.Parse(h.Endpoint)
	if err != nil {
		return "", fmt.Errorf("存储接口地址不正确: %w", err)
	}
	key := h.Prefix + "/" + time.Now().Format("20060102") + "/" + digest[:20] + extForMime(ref.Mime, ".png")
	host, path := ep.Host, "/"+h.Bucket+"/"+s3Escape(key)
	if h.VirtualHost {
		host, path = h.Bucket+"."+ep.Host, "/"+s3Escape(key)
	}

	now := time.Now().UTC()
	amzDate, day := now.Format("20060102T150405Z"), now.Format("20060102")
	sum := sha256.Sum256(ref.Data)
	payloadHash := hex.EncodeToString(sum[:])
	canonical := strings.Join([]string{
		http.MethodPut, path, "",
		"host:" + host, "x-amz-content-sha256:" + payloadHash, "x-amz-date:" + amzDate, "",
		"host;x-amz-content-sha256;x-amz-date", payloadHash,
	}, "\n")
	scope := day + "/" + h.Region + "/s3/aws4_request"
	crHash := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(crHash[:])
	signKey := hmacSHA256([]byte("AWS4"+h.SecretKey), day)
	for _, s := range []string{h.Region, "s3", "aws4_request"} {
		signKey = hmacSHA256(signKey, s)
	}
	sig := hex.EncodeToString(hmacSHA256(signKey, toSign))

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, ep.Scheme+"://"+host+path, bytes.NewReader(ref.Data))
	if err != nil {
		return "", err
	}
	req.Host = host
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("Content-Type", ref.Mime)
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=%s", h.AccessKey, scope, sig))
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("上传到自己的存储失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", fmt.Errorf("上传到自己的存储失败: HTTP %d %s", resp.StatusCode, clip(string(b), 300))
	}
	return h.PublicBase + "/" + s3Escape(key), nil
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// s3Escape 按 AWS 规则逐段编码对象路径（保留 /）。
func s3Escape(key string) string {
	segs := strings.Split(key, "/")
	for i, s := range segs {
		segs[i] = strings.ReplaceAll(url.PathEscape(s), "+", "%2B")
	}
	return strings.Join(segs, "/")
}

// needsURLRe 认出「接口只收图片网址、不收 base64」这类报错。
var needsURLRe = regexp.MustCompile(`(?i)(图片|参考图|首帧|尾帧|image|frame)[^,，。.;]{0,12}(url|网址|链接|地址)[^,，。.;]{0,8}(格式|不正确|无效|错误|invalid|not\s+valid|must)|(invalid|not\s+a\s+valid|unsupported)\s+(image\s+)?url|base64[^,，。.;]{0,10}(不支持|not\s+supported)|(仅|只)支持[^,，。.;]{0,6}(url|网址|链接)`)

func needsImageURL(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status/100 != 4 {
		return false
	}
	return needsURLRe.MatchString(innermostMessage(ae.Body))
}
