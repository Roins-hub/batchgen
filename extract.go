package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
)

const (
	stateRunning = "running"
	stateSuccess = "success"
	stateFailed  = "failed"
)

func parseJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("响应不是 JSON: %s", clip(string(data), 200))
	}
	return v, nil
}

// lookup 按 JSON 路径取值：a.b.0.c、a.b[0].c，* 表示数组或对象的所有元素。
func lookup(v any, p string) []any {
	p = strings.NewReplacer("[", ".", "]", "").Replace(strings.TrimSpace(p))
	cur := []any{v}
	for _, part := range strings.Split(p, ".") {
		if part == "" {
			continue
		}
		var next []any
		for _, c := range cur {
			switch x := c.(type) {
			case map[string]any:
				if part == "*" {
					for _, vv := range x {
						next = append(next, vv)
					}
				} else if vv, ok := x[part]; ok {
					next = append(next, vv)
				}
			case []any:
				if part == "*" {
					next = append(next, x...)
				} else if i, err := strconv.Atoi(part); err == nil && i >= 0 && i < len(x) {
					next = append(next, x[i])
				}
			}
		}
		cur = next
	}
	return cur
}

func scalarString(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case json.Number:
		return x.String()
	case bool:
		return strconv.FormatBool(x)
	}
	return ""
}

// lookupAll 依次尝试用 | 分隔的多个路径，返回第一个有值的路径的全部字符串结果。
func lookupAll(v any, paths string) []string {
	for _, p := range strings.Split(paths, "|") {
		if strings.TrimSpace(p) == "" {
			continue
		}
		var out []string
		for _, x := range lookup(v, p) {
			if s := scalarString(x); s != "" {
				out = append(out, s)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

func lookupFirst(v any, paths string) string {
	if all := lookupAll(v, paths); len(all) > 0 {
		return all[0]
	}
	return ""
}

func inList(val, list string) bool {
	for _, x := range strings.Split(list, ",") {
		if x = strings.ToLower(strings.TrimSpace(x)); x != "" && x == val {
			return true
		}
	}
	return false
}

// matchState 把状态值归类；成功/失败列表留空时用常见值。
func matchState(val, success, fail string) string {
	l := strings.ToLower(strings.TrimSpace(val))
	if l == "" {
		return stateRunning
	}
	if success != "" && inList(l, success) {
		return stateSuccess
	}
	if fail != "" && inList(l, fail) {
		return stateFailed
	}
	def := normState(l)
	if (def == stateSuccess && success == "") || (def == stateFailed && fail == "") {
		return def
	}
	return stateRunning
}

func normState(s string) string {
	switch strings.ToLower(s) {
	case "completed", "complete", "succeeded", "success", "succeed", "done", "finished":
		return stateSuccess
	case "failed", "failure", "fail", "error", "cancelled", "canceled", "expired", "rejected":
		return stateFailed
	}
	return stateRunning
}

func parseProgress(s string) int {
	s = strings.TrimSuffix(strings.TrimSpace(s), "%")
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	if f > 0 && f < 1 {
		f *= 100
	}
	return clampInt(int(f), 0, 100)
}

func errMsgAny(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	switch e := m["error"].(type) {
	case map[string]any:
		if s := lookupFirst(e, "message|msg"); s != "" {
			return s
		}
	case string:
		if e != "" {
			return e
		}
	}
	return lookupFirst(m, "fail_reason|failure_reason|message|msg|base_resp.status_msg|description")
}

// ---------- 识别「缺少参数」类报错，给出一键修复 ----------

// FixHint 记录接口要求补的参数，以及从报错里读到的可选值。
type FixHint struct {
	Param   string   `json:"param"`
	Options []string `json:"options,omitempty"`
}

var missingParamPatterns = []*regexp.Regexp{
	// 按秒计费模型必须提供时长（duration） / 请提供参数(seed) / 需要传入 xxx（size）
	regexp.MustCompile(`(?:必须|需要|请)(?:提供|传入|填写|指定|设置)[^（(\n]{0,20}[（(]\s*([A-Za-z_][\w.]*)\s*[）)]`),
	// mode 为必填项（references/frames）
	regexp.MustCompile(`([A-Za-z_][\w.]*)\s*(?:为必填项|是必填项|为必填|必填|不能为空|不可为空)\s*(?:[（(]([^）)]*)[）)])?`),
	// 缺少参数 mode / 缺少必填参数：mode
	regexp.MustCompile(`缺少(?:必填|必要)?(?:参数|字段)?[\s:：'"]*([A-Za-z_][\w.]*)`),
	// mode must be one of [references, frames] / mode should be one of: a, b
	regexp.MustCompile(`(?i)['"]?([A-Za-z_][\w.]*)['"]?\s+(?:must|should)\s+be\s+one\s+of[\s:]*\[?([^\]\n]+)\]?`),
	// missing required parameter: mode / missing field 'mode'
	regexp.MustCompile(`(?i)missing\s+(?:required\s+)?(?:parameter|param|field|argument|property)s?[\s:'"]*([A-Za-z_][\w.]*)`),
	// 'mode' is required / mode is a required field / duration is missing
	regexp.MustCompile(`(?i)['"]?([A-Za-z_][\w.]*)['"]?\s+(?:is\s+(?:a\s+)?required|field\s+is\s+required|is\s+missing)`),
	// must provide duration / please specify 'size'
	regexp.MustCompile(`(?i)(?:must|please)\s+(?:provide|specify|include|set)\s+(?:the\s+)?(?:parameter\s+|field\s+)?['"]?([A-Za-z_][\w.]*)`),
}

// detectMissingParam 从报错文字里找出缺的参数名和可选值；认不出返回 nil。
func detectMissingParam(msg string) *FixHint {
	for _, re := range missingParamPatterns {
		m := re.FindStringSubmatch(msg)
		if m == nil {
			continue
		}
		h := &FixHint{Param: m[1]}
		if lower := strings.ToLower(h.Param); lower == "field" || lower == "parameter" || lower == "param" {
			continue
		}
		if len(m) > 2 {
			for _, o := range regexp.MustCompile(`\s*(?:/|,|，|、|\||\bor\b|或)\s*`).Split(m[2], -1) {
				if o = strings.Trim(strings.TrimSpace(o), `'"`+"`"); o != "" && len(o) <= 40 && !strings.ContainsAny(o, " \t") {
					h.Options = append(h.Options, o)
				}
			}
		}
		return h
	}
	return nil
}

// fixHintFrom 只对接口拒绝请求参数（400/422）的情况给出修复建议。
func fixHintFrom(err error) *FixHint {
	var ae *APIError
	if !errors.As(err, &ae) || (ae.Status != 400 && ae.Status != 422) {
		return nil
	}
	return detectMissingParam(innermostMessage(ae.Body))
}

// ---------- 结果文件识别 ----------

type Output struct {
	URL, B64 string
	Auth     bool // 下载时是否带鉴权（地址属于本接口时）
}

var (
	dataURLRe = regexp.MustCompile(`data:[a-zA-Z]+/[a-zA-Z0-9.+-]+;base64,[A-Za-z0-9+/=]+`)
	mdImageRe = regexp.MustCompile(`!\[[^\]]*\]\((\S+?)\)`)
	httpURLRe = regexp.MustCompile(`https?://[^\s"'<>()\[\]]+`)
	base64Re  = regexp.MustCompile(`^[A-Za-z0-9+/=\r\n]+$`)
)

// mediaFromString 把一个字段值解析成文件：网址、data URL、纯 base64，或从一段文字里提取。
func mediaFromString(s string) []Output {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return nil
	case strings.HasPrefix(s, "data:"):
		return []Output{{B64: s}}
	case isHTTP(s) && !strings.ContainsAny(s, " \n\t"):
		return []Output{{URL: s}}
	}
	var outs []Output
	for _, m := range dataURLRe.FindAllString(s, -1) {
		outs = append(outs, Output{B64: m})
	}
	if md := mdImageRe.FindAllStringSubmatch(s, -1); len(md) > 0 {
		for _, m := range md {
			if isHTTP(m[1]) {
				outs = append(outs, Output{URL: m[1]})
			}
		}
	} else {
		for _, m := range httpURLRe.FindAllString(s, -1) {
			outs = append(outs, Output{URL: strings.TrimRight(m, ".,;:!?")})
		}
	}
	if len(outs) == 0 && len(s) >= 64 && base64Re.MatchString(s) {
		outs = append(outs, Output{B64: s})
	}
	return outs
}

func extractOutputs(v any, paths, kind string) []Output {
	var strs []string
	if strings.TrimSpace(paths) != "" {
		strs = lookupAll(v, paths)
	} else {
		strs = autoFind(v, kind)
	}
	var outs []Output
	seen := map[string]bool{}
	for _, s := range strs {
		for _, o := range mediaFromString(s) {
			key := o.URL + clip(o.B64, 80)
			if !seen[key] {
				seen[key] = true
				outs = append(outs, o)
			}
		}
	}
	return outs
}

// autoFind 没配置结果字段时，在整个响应里找最像结果文件的值。
func autoFind(v any, kind string) []string {
	var best, fallback []string
	var walk func(v any, key string)
	walk = func(v any, key string) {
		switch x := v.(type) {
		case map[string]any:
			for k, vv := range x {
				walk(vv, k)
			}
		case []any:
			for _, vv := range x {
				walk(vv, key)
			}
		case string:
			k := strings.ToLower(key)
			switch {
			case strings.HasPrefix(x, "data:"):
				best = append(best, x)
			case isHTTP(x):
				if mediaExtMatches(x, kind) {
					best = append(best, x)
				} else if strings.Contains(k, "url") && !strings.Contains(k, "cover") &&
					!strings.Contains(k, "thumb") && !strings.Contains(k, "poster") &&
					(kind != KindVideo || (!strings.Contains(k, "image") && !strings.Contains(k, "frame"))) {
					fallback = append(fallback, x)
				}
			case k == "b64_json" || k == "bytesbase64encoded" || k == "base64":
				best = append(best, x)
			}
		}
	}
	walk(v, "")
	if len(best) > 0 {
		return best
	}
	if len(fallback) > 0 {
		return fallback[:1]
	}
	return nil
}

func mediaExtMatches(raw, kind string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	switch strings.ToLower(path.Ext(u.Path)) {
	case ".mp4", ".webm", ".mov", ".m4v":
		return kind == KindVideo
	case ".png", ".jpg", ".jpeg", ".webp", ".gif":
		return kind == KindImage
	}
	return false
}
