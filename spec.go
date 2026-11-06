package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Request 描述一次提交请求。路径里可用 {model}；请求体是 JSON 模板，变量写作 {{prompt}} 等。
type Request struct {
	Method        string `json:"method"`
	Path          string `json:"path"`
	BodyType      string `json:"body_type"` // json | form
	Body          string `json:"body"`
	FileField     string `json:"file_field"`      // form 时参考图（首帧）文件的字段名
	FileMulti     bool   `json:"file_multi"`      // form 时接口接受多张参考图（字段名自动加 []）
	FileFieldLast string `json:"file_field_last"` // form 时尾帧文件的字段名，可与上面相同（按顺序上传两个文件）
}

// Poll 描述异步任务的查询方式。字段都是 JSON 路径，如 data.task_status、data.videos.*.url。
type Poll struct {
	Path     string `json:"path"`     // 可用 {id}、{model}
	PathRef  string `json:"path_ref"` // 用参考图提交时改用的查询路径（可灵）
	Status   string `json:"status"`
	Success  string `json:"success"` // 逗号分隔，留空用常见值
	Fail     string `json:"fail"`
	Progress string `json:"progress"`
	Error    string `json:"error"`
	FileID   string `json:"file_id"` // 成功后取文件 ID，供 Content 里的 {file_id} 使用
	Content  string `json:"content"` // 结果里没有文件地址时的下载路径，可用 {id}、{file_id}
}

// Spec 是一种接口格式的完整描述；内置预设和自定义接口都用它。
type Spec struct {
	Preset  string            `json:"preset"`
	BaseURL string            `json:"base_url"` // 留空用通用接口地址
	APIKeys []string          `json:"api_keys"` // 留空用通用 Key
	Auth    string            `json:"auth"`     // bearer | x-goog-api-key | x-api-key | kling_jwt | none
	Headers map[string]string `json:"headers"`
	// 本地参考图怎么发：inline = 直接把图片数据（base64）放进请求；upload = 先传到图床，发送网址
	ImageDelivery string  `json:"image_delivery"`
	Submit        Request `json:"submit"`
	SubmitRef     Request `json:"submit_ref"` // 有参考图时改用的请求；路径留空表示沿用 Submit
	TaskID        string  `json:"task_id"`    // 留空 = 同步接口，提交响应里直接取结果
	Result        string  `json:"result"`     // 结果文件（网址或 base64）的 JSON 路径，留空自动识别
	Poll          Poll    `json:"poll"`
}

// JobPoll 在提交成功时存进任务记录，之后改设置不影响已提交任务的查询。
type JobPoll struct {
	Path     string            `json:"path"`
	Status   string            `json:"status,omitempty"`
	Success  string            `json:"success,omitempty"`
	Fail     string            `json:"fail,omitempty"`
	Progress string            `json:"progress,omitempty"`
	Error    string            `json:"error,omitempty"`
	FileID   string            `json:"file_id,omitempty"`
	Content  string            `json:"content,omitempty"`
	Result   string            `json:"result,omitempty"`
	Auth     string            `json:"auth,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
}

func (s Spec) clone() Spec {
	s.APIKeys = slices.Clone(s.APIKeys)
	s.Headers = maps.Clone(s.Headers)
	return s
}

func (r *Request) normalize() {
	r.Method = strings.ToUpper(strings.TrimSpace(r.Method))
	if r.Method == "" {
		r.Method = "POST"
	}
	r.Path = normPath(r.Path)
	if r.BodyType != "form" {
		r.BodyType = "json"
	}
	r.Body = strings.TrimSpace(r.Body)
	r.FileField = strings.TrimSpace(r.FileField)
	r.FileFieldLast = strings.TrimSpace(r.FileFieldLast)
}

func (s *Spec) normalize() {
	s.Preset = strings.TrimSpace(s.Preset)
	s.BaseURL = normBase(s.BaseURL)
	s.APIKeys = cleanKeys(s.APIKeys)
	switch s.Auth {
	case "bearer", "x-goog-api-key", "x-api-key", "kling_jwt", "none":
	default:
		s.Auth = "bearer"
	}
	headers := map[string]string{}
	for k, v := range s.Headers {
		if k = strings.TrimSpace(k); k != "" {
			headers[k] = strings.TrimSpace(v)
		}
	}
	s.Headers = headers
	if s.ImageDelivery != "upload" {
		s.ImageDelivery = "inline"
	}
	s.Submit.normalize()
	s.SubmitRef.normalize()
	if s.SubmitRef.Path == "" {
		s.SubmitRef = Request{}
	}
	s.TaskID = strings.TrimSpace(s.TaskID)
	s.Result = strings.TrimSpace(s.Result)
	p := &s.Poll
	p.Path, p.PathRef, p.Content = normPath(p.Path), normPath(p.PathRef), normPath(p.Content)
	for _, f := range []*string{&p.Status, &p.Success, &p.Fail, &p.Progress, &p.Error, &p.FileID} {
		*f = strings.TrimSpace(*f)
	}
}

func (s Spec) validate(name string) error {
	if s.Submit.Path == "" {
		return fmt.Errorf("%s接口：提交路径不能为空", name)
	}
	for _, r := range []Request{s.Submit, s.SubmitRef} {
		if r.Path == "" || r.Body == "" {
			continue
		}
		var v any
		if err := json.Unmarshal([]byte(r.Body), &v); err != nil {
			return fmt.Errorf("%s接口：请求体模板不是合法的 JSON（%s）：%v", name, r.Path, err)
		}
		if _, ok := v.(map[string]any); !ok && r.BodyType == "form" {
			return fmt.Errorf("%s接口：表单格式的请求体模板必须是 JSON 对象", name)
		}
		if bad := unknownVars(r.Body); bad != "" {
			return fmt.Errorf("%s接口：请求体模板里有未知变量 {{%s}}", name, bad)
		}
	}
	if s.TaskID != "" && s.Poll.Path == "" {
		return fmt.Errorf("%s接口：填了任务 ID 字段（异步接口）就必须填查询路径", name)
	}
	return nil
}

// requestUsesImage 判断请求会不会把参考图发出去。
func requestUsesImage(r Request) bool {
	for _, v := range []string{"{{image", "{{references_image", "{{frames_first", `"$each"`} {
		if strings.Contains(r.Body, v) {
			return true
		}
	}
	return r.BodyType == "form" && r.FileField != ""
}

// acceptsManyRefs 判断这个接口格式能不能一次发多张参考图。
func (s Spec) acceptsManyRefs() bool {
	r := s.Submit
	if s.SubmitRef.Path != "" {
		r = s.SubmitRef
	}
	for _, v := range []string{"{{images", "{{references_images", `"$each"`} {
		if strings.Contains(r.Body, v) {
			return true
		}
	}
	return r.BodyType == "form" && (r.FileMulti || strings.HasSuffix(r.FileField, "[]"))
}

// refreshPreset 让内置格式跟上程序版本：配置里存的是当时的副本，新版改进的请求写法要同步过去。
// 地址、Key、请求头和参考图发送方式是用户的设置，不动；改过模板的格式是「自定义」，也不动。
func (s *Spec) refreshPreset() {
	if p, ok := presetByID(s.Preset); ok && s.Preset != "custom" {
		s.Submit, s.SubmitRef, s.TaskID, s.Result, s.Poll = p.Submit, p.SubmitRef, p.TaskID, p.Result, p.Poll
	}
}

// needsRefData 判断是否需要把网址形式的参考图先下载下来（要 base64 或上传文件时）。
func (s Spec) needsRefData() bool {
	for _, r := range []Request{s.Submit, s.SubmitRef} {
		if strings.Contains(r.Body, "_b64") || strings.Contains(r.Body, "_mime") ||
			(r.BodyType == "form" && (r.FileField != "" || r.FileFieldLast != "")) {
			return true
		}
	}
	return false
}

// ---------- 内置预设 ----------

type Preset struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Desc     string `json:"desc"`
	BaseHint string `json:"base_hint"`
	Spec     Spec   `json:"spec"`
}

func presetByID(id string) (Spec, bool) {
	for _, p := range presets {
		if p.ID == id {
			s := p.Spec.clone()
			s.Preset = p.ID
			s.normalize()
			return s, true
		}
	}
	return Spec{}, false
}

var presets = []Preset{
	// ---------------- 图片 ----------------
	{
		ID: "openai_images", Kind: KindImage, Name: "OpenAI 图片接口",
		Desc: "/v1/images/generations，带参考图时走 /v1/images/edits。适用于 gpt-image、dall-e 以及大多数 OpenAI 兼容中转站。",
		Spec: Spec{
			Submit: Request{Path: "/v1/images/generations", Body: `{
  "model": "{{model}}",
  "prompt": "{{prompt}}",
  "n": 1,
  "size": "{{size}}"
}`},
			SubmitRef: Request{Path: "/v1/images/edits", BodyType: "form", FileField: "image", FileMulti: true, Body: `{
  "model": "{{model}}",
  "prompt": "{{prompt}}",
  "n": "1",
  "size": "{{size}}"
}`},
			Result: "data.*.b64_json|data.*.url",
		},
	},
	{
		ID: "openai_chat", Kind: KindImage, Name: "Chat 对话出图",
		Desc: "/v1/chat/completions，适用于 Gemini 2.5 Flash Image（nano-banana）、GPT-4o 等通过对话返回图片的模型。图片从回复内容或 images 字段里自动提取。",
		Spec: Spec{
			Submit: Request{Path: "/v1/chat/completions", Body: `{
  "model": "{{model}}",
  "stream": false,
  "messages": [
    {
      "role": "user",
      "content": [
        { "type": "text", "text": "{{prompt}}[[\n画面尺寸/比例：{{size}}]]" },
        { "$each": "refs", "type": "image_url", "image_url": { "url": "{{item}}" } }
      ]
    }
  ]
}`},
			Result: "choices.0.message.images.*.image_url.url|choices.0.message.content",
		},
	},
	{
		ID: "gemini_image", Kind: KindImage, Name: "Gemini 原生（generateContent）",
		Desc:     "Google Gemini 原生接口 /v1beta/models/{model}:generateContent，鉴权用 x-goog-api-key。",
		BaseHint: "https://generativelanguage.googleapis.com",
		Spec: Spec{
			Auth: "x-goog-api-key",
			Submit: Request{Path: "/v1beta/models/{model}:generateContent", Body: `{
  "contents": [
    {
      "role": "user",
      "parts": [
        { "text": "{{prompt}}" },
        { "$each": "refs", "inline_data": { "mime_type": "{{item_mime}}", "data": "{{item_b64}}" } }
      ]
    }
  ],
  "generationConfig": {
    "responseModalities": ["TEXT", "IMAGE"],
    "imageConfig": { "aspectRatio": "{{aspect_ratio}}" }
  }
}`},
			Result: "candidates.0.content.parts.*.inlineData.data|candidates.0.content.parts.*.inline_data.data",
		},
	},
	{
		ID: "seedream", Kind: KindImage, Name: "火山方舟 Seedream（即梦图片）",
		Desc:     "火山方舟 /api/v3/images/generations，模型如 doubao-seedream-4-0。尺寸可填 1024x1024 或 2K。",
		BaseHint: "https://ark.cn-beijing.volces.com",
		Spec: Spec{
			Submit: Request{Path: "/api/v3/images/generations", Body: `{
  "model": "{{model}}",
  "prompt": "{{prompt}}",
  "size": "{{size}}",
  "image": "{{images}}",
  "response_format": "url",
  "watermark": false
}`},
			Result: "data.*.url|data.*.b64_json",
		},
	},
	{
		ID: "kling_image", Kind: KindImage, Name: "可灵 图片生成",
		Desc:     "可灵官方 /v1/images/generations（异步）。官方 Key 填 AccessKey:SecretKey 会自动生成 JWT；中转站的 sk Key 直接填即可。",
		BaseHint: "https://api-beijing.klingai.com",
		Spec: Spec{
			Auth: "kling_jwt",
			Submit: Request{Path: "/v1/images/generations", Body: `{
  "model_name": "{{model}}",
  "prompt": "{{prompt}}",
  "image": "{{image_url_or_b64}}",
  "aspect_ratio": "{{aspect_ratio}}",
  "n": 1
}`},
			TaskID: "data.task_id",
			Result: "data.task_result.images.*.url",
			Poll: Poll{Path: "/v1/images/generations/{id}", Status: "data.task_status",
				Success: "succeed", Fail: "failed", Error: "data.task_status_msg|message"},
		},
	},
	{
		ID: "wan_image", Kind: KindImage, Name: "阿里百炼 通义万相 文生图",
		Desc:     "DashScope 异步接口 text2image/image-synthesis，尺寸会转成 1024*1024 的写法。不支持参考图。",
		BaseHint: "https://dashscope.aliyuncs.com",
		Spec: Spec{
			Headers: map[string]string{"X-DashScope-Async": "enable"},
			Submit: Request{Path: "/api/v1/services/aigc/text2image/image-synthesis", Body: `{
  "model": "{{model}}",
  "input": { "prompt": "{{prompt}}" },
  "parameters": { "size": "{{size_star}}", "n": 1 }
}`},
			TaskID: "output.task_id",
			Result: "output.results.*.url",
			Poll: Poll{Path: "/api/v1/tasks/{id}", Status: "output.task_status",
				Success: "succeeded", Fail: "failed,canceled,unknown", Error: "output.message|message"},
		},
	},
	{
		ID: "midjourney", Kind: KindImage, Name: "Midjourney（MJ Proxy / 中转站 /mj）",
		Desc: "midjourney-proxy 格式：/mj/submit/imagine 提交，/mj/task/{id}/fetch 查询。尺寸填 16:9 这类比例会自动加 --ar。",
		Spec: Spec{
			Submit: Request{Path: "/mj/submit/imagine", Body: `{
  "prompt": "{{prompt}}[[ --ar {{aspect_ratio}}]]",
  "base64Array": ["{{image}}"]
}`},
			TaskID: "result",
			Result: "imageUrl",
			Poll: Poll{Path: "/mj/task/{id}/fetch", Status: "status", Success: "success", Fail: "failure",
				Progress: "progress", Error: "failReason|description"},
		},
	},

	// ---------------- 视频 ----------------
	{
		ID: "newapi_video", Kind: KindVideo, Name: "NewAPI 统一视频格式",
		Desc: "New API / One API 类中转站的 /v1/video/generations，结果地址自动识别。",
		Spec: Spec{
			Submit: Request{Path: "/v1/video/generations", Body: `{
  "model": "{{model}}",
  "prompt": "{{prompt}}",
  "size": "{{size}}",
  "duration": "{{duration}}",
  "image": "{{image}}",
  "images": "{{images}}",
  "image_tail": "{{image_last}}"
}`},
			TaskID: "task_id|id|data.task_id",
			Poll: Poll{Path: "/v1/video/generations/{id}", Status: "status|data.status|state",
				Progress: "progress|data.progress", Error: "error.message|fail_reason|data.fail_reason"},
		},
	},
	{
		ID: "sora", Kind: KindVideo, Name: "OpenAI Sora（/v1/videos）",
		Desc: "OpenAI 视频接口：完成后从 /v1/videos/{id}/content 下载。带参考图时用表单上传 input_reference。",
		Spec: Spec{
			Submit: Request{Path: "/v1/videos", Body: `{
  "model": "{{model}}",
  "prompt": "{{prompt}}",
  "seconds": "{{duration_str}}",
  "size": "{{size}}"
}`},
			SubmitRef: Request{Path: "/v1/videos", BodyType: "form", FileField: "input_reference", FileFieldLast: "input_reference", Body: `{
  "model": "{{model}}",
  "prompt": "{{prompt}}",
  "seconds": "{{duration_str}}",
  "size": "{{size}}"
}`},
			TaskID: "id",
			Result: "url|video_url",
			Poll: Poll{Path: "/v1/videos/{id}", Status: "status", Success: "completed", Fail: "failed,cancelled",
				Progress: "progress", Error: "error.message", Content: "/v1/videos/{id}/content"},
		},
	},
	{
		ID: "openai_video_seedance", Kind: KindVideo, Name: "OpenAI 视频 · Seedance 扩展（JSON）",
		Desc: "New API 等中转站的 Seedance 2.x 渠道（模型类型 openai-video）：/v1/videos，JSON 请求体，用 mode + references / frames 传参考图和首尾帧，自动带 client_task_id 和 duration。本地参考图直接随请求发送，渠道只收网址时会自动改为先传图床。",
		Spec: Spec{
			Submit: Request{Path: "/v1/videos", Body: `{
  "model": "{{model}}",
  "prompt": "{{prompt}}",
  "mode": "{{mode}}",
  "aspect_ratio": "{{aspect_ratio}}",
  "duration": "{{duration}}",
  "client_task_id": "{{uuid}}",
  "references": { "image": "{{references_image}}", "images": "{{references_images}}" },
  "frames": { "first_frame": "{{frames_first}}", "last_frame": "{{frames_last}}" }
}`},
			TaskID: "id",
			Result: "url|video_url",
			Poll: Poll{Path: "/v1/videos/{id}", Status: "status", Success: "completed", Fail: "failed,cancelled",
				Progress: "progress", Error: "error.message", Content: "/v1/videos/{id}/content"},
		},
	},
	{
		ID: "kling_video", Kind: KindVideo, Name: "可灵 Kling",
		Desc:     "可灵官方 text2video / image2video。官方 Key 填 AccessKey:SecretKey 会自动生成 JWT；走中转站时把路径改成中转站的（如 /kling/v1/...）。",
		BaseHint: "https://api-beijing.klingai.com",
		Spec: Spec{
			Auth: "kling_jwt",
			Submit: Request{Path: "/v1/videos/text2video", Body: `{
  "model_name": "{{model}}",
  "prompt": "{{prompt}}",
  "duration": "{{duration_str}}",
  "aspect_ratio": "{{aspect_ratio}}",
  "mode": "std"
}`},
			SubmitRef: Request{Path: "/v1/videos/image2video", Body: `{
  "model_name": "{{model}}",
  "prompt": "{{prompt}}",
  "image": "{{image_url_or_b64}}",
  "image_tail": "{{image_last_url_or_b64}}",
  "duration": "{{duration_str}}",
  "mode": "std"
}`},
			TaskID: "data.task_id",
			Result: "data.task_result.videos.*.url",
			Poll: Poll{Path: "/v1/videos/text2video/{id}", PathRef: "/v1/videos/image2video/{id}",
				Status: "data.task_status", Success: "succeed", Fail: "failed", Error: "data.task_status_msg|message"},
		},
	},
	{
		ID: "seedance", Kind: KindVideo, Name: "火山方舟 Seedance（即梦视频）",
		Desc:     "火山方舟 /api/v3/contents/generations/tasks，模型如 doubao-seedance-1-0-pro。参考图作为首帧。",
		BaseHint: "https://ark.cn-beijing.volces.com",
		Spec: Spec{
			Submit: Request{Path: "/api/v3/contents/generations/tasks", Body: `{
  "model": "{{model}}",
  "content": [
    { "type": "text", "text": "{{prompt}}" },
    { "$if": "image", "type": "image_url", "image_url": { "url": "{{image}}" }, "role": "first_frame" },
    { "$if": "image_last", "type": "image_url", "image_url": { "url": "{{image_last}}" }, "role": "last_frame" }
  ],
  "ratio": "{{aspect_ratio}}",
  "duration": "{{duration}}",
  "resolution": "{{resolution}}",
  "watermark": false
}`},
			TaskID: "id",
			Result: "content.video_url",
			Poll: Poll{Path: "/api/v3/contents/generations/tasks/{id}", Status: "status",
				Success: "succeeded", Fail: "failed,expired,cancelled", Error: "error.message"},
		},
	},
	{
		ID: "wan_video", Kind: KindVideo, Name: "阿里百炼 通义万相 视频",
		Desc:     "DashScope 异步接口 video-generation/video-synthesis。文生视频用 t2v 模型，带参考图时用 i2v 模型。",
		BaseHint: "https://dashscope.aliyuncs.com",
		Spec: Spec{
			Headers: map[string]string{"X-DashScope-Async": "enable"},
			Submit: Request{Path: "/api/v1/services/aigc/video-generation/video-synthesis", Body: `{
  "model": "{{model}}",
  "input": { "prompt": "{{prompt}}", "img_url": "{{image}}" },
  "parameters": {
    "resolution": "{{resolution_upper}}",
    "ratio": "{{aspect_ratio}}",
    "duration": "{{duration}}"
  }
}`},
			TaskID: "output.task_id",
			Result: "output.video_url",
			Poll: Poll{Path: "/api/v1/tasks/{id}", Status: "output.task_status",
				Success: "succeeded", Fail: "failed,canceled,unknown", Error: "output.message|message"},
		},
	},
	{
		ID: "minimax_v1", Kind: KindVideo, Name: "MiniMax 海螺（v1）",
		Desc:     "Hailuo-02 / 2.3 等：/v1/video_generation 提交，成功后拿 file_id 再到 /v1/files/retrieve 取下载地址。",
		BaseHint: "https://api.minimaxi.com",
		Spec: Spec{
			Submit: Request{Path: "/v1/video_generation", Body: `{
  "model": "{{model}}",
  "prompt": "{{prompt}}",
  "duration": "{{duration}}",
  "resolution": "{{resolution_upper}}",
  "first_frame_image": "{{image}}",
  "last_frame_image": "{{image_last}}"
}`},
			TaskID: "task_id",
			Poll: Poll{Path: "/v1/query/video_generation?task_id={id}", Status: "status", Success: "success",
				Fail: "fail", Error: "base_resp.status_msg", FileID: "file_id",
				Content: "/v1/files/retrieve?file_id={file_id}"},
		},
	},
	{
		ID: "minimax_v2", Kind: KindVideo, Name: "MiniMax（v2 / H3）",
		Desc:     "MiniMax 新版 /v2/video_generation，查询结果里直接给视频地址。",
		BaseHint: "https://api.minimaxi.com",
		Spec: Spec{
			Submit: Request{Path: "/v2/video_generation", Body: `{
  "model": "{{model}}",
  "content": [
    { "type": "text", "text": "{{prompt}}" },
    { "$if": "image", "type": "image_url", "image_url": { "url": "{{image}}" }, "role": "first_frame" },
    { "$if": "image_last", "type": "image_url", "image_url": { "url": "{{image_last}}" }, "role": "last_frame" }
  ],
  "duration": "{{duration}}",
  "resolution": "{{resolution_upper}}",
  "ratio": "{{aspect_ratio}}"
}`},
			TaskID: "task_id",
			Result: "task.content.url",
			Poll: Poll{Path: "/v2/query/video_generation/{id}", Status: "task.status", Success: "succeeded",
				Fail: "failed,cancelled", Error: "task.error.message|task.error|base_resp.status_msg"},
		},
	},
	{
		ID: "veo", Kind: KindVideo, Name: "Google Veo（Gemini API）",
		Desc:     "Gemini API 的 predictLongRunning，鉴权用 x-goog-api-key；完成标志是 done=true。",
		BaseHint: "https://generativelanguage.googleapis.com",
		Spec: Spec{
			Auth: "x-goog-api-key",
			Submit: Request{Path: "/v1beta/models/{model}:predictLongRunning", Body: `{
  "instances": [
    {
      "prompt": "{{prompt}}",
      "image": { "bytesBase64Encoded": "{{image_b64}}", "mimeType": "{{image_mime}}" },
      "lastFrame": { "bytesBase64Encoded": "{{image_last_b64}}", "mimeType": "{{image_last_mime}}" }
    }
  ],
  "parameters": {
    "aspectRatio": "{{aspect_ratio}}",
    "durationSeconds": "{{duration}}",
    "resolution": "{{resolution}}",
    "sampleCount": 1
  }
}`},
			TaskID: "name",
			Result: "response.generateVideoResponse.generatedSamples.*.video.uri",
			Poll:   Poll{Path: "/v1beta/{id}", Status: "done", Success: "true", Error: "error.message"},
		},
	},
}

// customTemplate 是选择「自定义」时的起点：一个常见的异步接口写法。
var customTemplate = Spec{
	Preset: "custom",
	Submit: Request{Path: "/v1/your/submit", Body: `{
  "model": "{{model}}",
  "prompt": "{{prompt}}",
  "image": "{{image}}"
}`},
	TaskID: "task_id",
	Poll:   Poll{Path: "/v1/your/query/{id}", Status: "status", Progress: "progress", Error: "error.message|message"},
}
