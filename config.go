package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

type Config struct {
	BaseURL          string   `json:"base_url"`
	APIKeys          []string `json:"api_keys"`
	ModelsPath       string   `json:"models_path"` // 「测试连接」用的模型列表路径
	ImageModel       string   `json:"image_model"`
	VideoModel       string   `json:"video_model"`
	ImageSpec        Spec     `json:"image_spec"`
	VideoSpec        Spec     `json:"video_spec"`
	ImageConcurrency int      `json:"image_concurrency"`
	VideoConcurrency int      `json:"video_concurrency"`
	PollIntervalSec  int      `json:"poll_interval_sec"`
	VideoTimeoutMin  int      `json:"video_timeout_min"` // 异步任务（图片和视频）的超时
	MaxRetries       int      `json:"max_retries"`
	MaxRefImages     int      `json:"max_ref_images"` // 每个任务最多上传几张参考图
	OutputDir        string   `json:"output_dir"`

	ExtraParams []ExtraParam `json:"extra_params"` // 按模型附加到请求体里的参数
	ManualFix   bool         `json:"manual_fix"`   // true = 缺参数时不自动补，交给用户在卡片上选
	ImageHost   ImageHost    `json:"image_host"`   // 参考图上传到哪里（接口只收网址时用）

	UpdateMirror string `json:"update_mirror"` // 自动更新的镜像地址，GitHub 地址拼在它后面
	ProxyURL     string `json:"proxy_url"`     // 网络代理，优先于系统代理

	LegacyVideoAPI string `json:"video_api,omitempty"` // 旧版配置，仅用于迁移
}

// ExtraParam 是一条附加参数规则：提交某类（某个模型）的任务时，往请求体里加上 Key=Value。
// 中转站常有自己要求的字段（如 mode），用它就不必改请求体模板。
type ExtraParam struct {
	Kind  string `json:"kind"`  // image | video
	Model string `json:"model"` // 留空 = 该类型的所有模型
	Key   string `json:"key"`   // 可写 a.b 表示嵌套字段
	Value string `json:"value"` // true/false/数字按原类型发送，其余按字符串；用英文双引号包起来可强制为字符串
}

func normExtraParams(in []ExtraParam) []ExtraParam {
	var out []ExtraParam
	for _, e := range in {
		e.Kind, e.Model, e.Key = strings.TrimSpace(e.Kind), strings.TrimSpace(e.Model), strings.TrimSpace(e.Key)
		e.Value = strings.TrimSpace(e.Value)
		if e.Key == "" {
			continue
		}
		if e.Kind != KindImage {
			e.Kind = KindVideo
		}
		// 同类型、同模型、同参数只保留最后一条
		out = slices.DeleteFunc(out, func(x ExtraParam) bool { return x.Kind == e.Kind && x.Model == e.Model && x.Key == e.Key })
		out = append(out, e)
	}
	return out
}

// extrasFor 返回作用于某个模型的附加参数；针对具体模型的规则覆盖「所有模型」的规则。
func extrasFor(params []ExtraParam, kind, model string) []ExtraParam {
	var all, specific []ExtraParam
	for _, e := range params {
		if e.Kind != kind {
			continue
		}
		switch e.Model {
		case "":
			all = append(all, e)
		case model:
			specific = append(specific, e)
		}
	}
	return append(all, specific...)
}

func defaultConfig() Config {
	return Config{
		ModelsPath:       "/v1/models",
		ImageModel:       "gpt-image-1",
		VideoModel:       "sora-2",
		ImageConcurrency: 5,
		VideoConcurrency: 10,
		PollIntervalSec:  5,
		VideoTimeoutMin:  30,
		MaxRetries:       2,
		MaxRefImages:     4,
	}
}

// maxConcurrency 是同时生成数量的上限；接口限流时（429）会自动等待重试。
const maxConcurrency = 50

// refImagesLimit 是「参考图最多张数」设置的上限。
const refImagesLimit = 20

func clampInt(v, lo, hi int) int {
	return max(lo, min(hi, v))
}

func normBase(s string) string {
	s = strings.TrimRight(strings.TrimSpace(s), "/")
	s = strings.TrimSuffix(s, "/v1")
	if s != "" && !strings.Contains(s, "://") {
		s = "http://" + s
	}
	return s
}

func cleanKeys(in []string) []string {
	var keys []string
	for _, k := range in {
		if k = strings.TrimSpace(k); k != "" && !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	return keys
}

func normPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || isHTTP(p) {
		return p
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

func (c *Config) normalize() {
	c.BaseURL = normBase(c.BaseURL)
	c.APIKeys = cleanKeys(c.APIKeys)
	c.ModelsPath = firstNonEmpty(normPath(c.ModelsPath), "/v1/models")
	c.ImageModel = strings.TrimSpace(c.ImageModel)
	c.VideoModel = strings.TrimSpace(c.VideoModel)

	// 没配置过接口格式（新装或旧版配置）时套用预设。
	if c.ImageSpec.Submit.Path == "" {
		c.ImageSpec, _ = presetByID("openai_images")
	}
	if c.VideoSpec.Submit.Path == "" {
		// 默认用 /v1/videos：New API 上绑定任务插件的 Seedance 渠道不接受 /v1/video/generations
		id := "openai_video_seedance"
		if c.LegacyVideoAPI == "sora" {
			id = "sora"
		}
		c.VideoSpec, _ = presetByID(id)
	}
	c.LegacyVideoAPI = ""
	c.ImageSpec.normalize()
	c.VideoSpec.normalize()
	c.ImageSpec.refreshPreset()
	c.VideoSpec.refreshPreset()

	c.ImageConcurrency = clampInt(c.ImageConcurrency, 1, maxConcurrency)
	c.VideoConcurrency = clampInt(c.VideoConcurrency, 1, maxConcurrency)
	c.PollIntervalSec = clampInt(c.PollIntervalSec, 2, 120)
	c.VideoTimeoutMin = clampInt(c.VideoTimeoutMin, 1, 240)
	c.MaxRetries = clampInt(c.MaxRetries, 0, 5)
	c.MaxRefImages = clampInt(c.MaxRefImages, 1, refImagesLimit)
	c.OutputDir = strings.TrimSpace(c.OutputDir)
	c.ProxyURL = strings.TrimSpace(c.ProxyURL)
	if c.ProxyURL != "" && !strings.Contains(c.ProxyURL, "://") {
		c.ProxyURL = "http://" + c.ProxyURL
	}
	c.UpdateMirror = strings.TrimSpace(c.UpdateMirror)
	if c.UpdateMirror != "" && !strings.HasSuffix(c.UpdateMirror, "/") {
		c.UpdateMirror += "/"
	}
	c.ExtraParams = normExtraParams(c.ExtraParams)
	c.ImageHost.normalize()
}

func (c Config) validate() error {
	if c.ProxyURL != "" {
		u, err := url.Parse(c.ProxyURL)
		if err != nil || u.Host == "" || !slices.Contains([]string{"http", "https", "socks5", "socks5h"}, u.Scheme) {
			return errors.New("网络代理地址不正确，例如 127.0.0.1:7890 或 socks5://127.0.0.1:1080")
		}
	}
	if err := c.ImageSpec.validate("图片"); err != nil {
		return err
	}
	return c.VideoSpec.validate("视频")
}

func (c Config) defaultModel(kind string) string {
	if kind == KindVideo {
		return c.VideoModel
	}
	return c.ImageModel
}

func (c Config) Spec(kind string) Spec {
	if kind == KindVideo {
		return c.VideoSpec.clone()
	}
	return c.ImageSpec.clone()
}

// endpoint 返回某类任务实际使用的接口地址和 Key：接口格式里单独填了就用单独的，否则用通用的。
func (c Config) endpoint(kind string) (string, []string) {
	s := c.Spec(kind)
	keys := s.APIKeys
	if len(keys) == 0 {
		keys = c.APIKeys
	}
	return firstNonEmpty(s.BaseURL, c.BaseURL), keys
}

type ConfigStore struct {
	mu      sync.RWMutex
	path    string
	dataDir string
	cfg     Config
}

func LoadConfig(path, dataDir string) (*ConfigStore, error) {
	cs := &ConfigStore{path: path, dataDir: dataDir, cfg: defaultConfig()}
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal(b, &cs.cfg); err != nil {
			return nil, fmt.Errorf("config.json 格式错误: %w", err)
		}
	}
	cs.cfg.normalize()
	setManualProxy(cs.cfg.ProxyURL)
	return cs, nil
}

func (cs *ConfigStore) Get() Config {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	c := cs.cfg
	c.APIKeys = slices.Clone(c.APIKeys)
	c.ImageSpec = c.ImageSpec.clone()
	c.VideoSpec = c.VideoSpec.clone()
	c.ExtraParams = slices.Clone(c.ExtraParams)
	return c
}

// Set 保存配置；c 应已 normalize 并通过 validate。
func (cs *ConfigStore) Set(c Config) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if err := writeJSONAtomic(cs.path, c); err != nil {
		return err
	}
	cs.cfg = c
	setManualProxy(c.ProxyURL)
	return nil
}

// OutputRoot 返回输出根目录的绝对路径；留空时放在程序旁的 outputs 目录。
func (cs *ConfigStore) OutputRoot(c Config) string {
	if c.OutputDir == "" {
		return filepath.Join(cs.dataDir, "outputs")
	}
	if filepath.IsAbs(c.OutputDir) {
		return c.OutputDir
	}
	return filepath.Join(cs.dataDir, c.OutputDir)
}
