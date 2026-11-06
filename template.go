package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// 请求体模板规则：
//   - {{变量}} 会被替换；整个字符串只有 {{duration}}/{{width}}/{{height}} 时替换成数字。
//   - 字符串里用到的变量为空时，这个键（或数组元素）整个去掉；对象因此变空时也去掉。
//   - [[ ... ]] 是可选片段：里面任何变量为空，整段删掉，例如 "{{prompt}}[[ --ar {{aspect_ratio}}]]"。
//   - 对象里写 "$if": "image" 表示只有该变量非空时才保留这个对象（用于数组里的图片元素）。
//   - 数组里的对象写 "$each": "refs" 表示每张参考图生成一个元素，里面用 {{item}} {{item_b64}} {{item_mime}}。
//   - {{images}} 单独作为值时是所有参考图组成的数组。

var templateVars = []string{
	"prompt", "model", "size", "size_star", "width", "height", "aspect_ratio",
	"resolution", "resolution_upper", "duration", "duration_str",
	"image", "image_b64", "image_mime", "image_url_or_b64",
	"image_last", "image_last_b64", "image_last_mime", "image_last_url_or_b64",
	"uuid", "job_id",
	"mode", "references_image", "frames_first", "frames_last",
	"images", "references_images", "item", "item_b64", "item_mime", "item_url_or_b64",
}

var numericVars = map[string]bool{"duration": true, "width": true, "height": true}

// listVars 单独占一个值时渲染成 JSON 数组。
var listVars = map[string]bool{"images": true, "references_images": true}

var (
	tokenRe = regexp.MustCompile(`(?s)\[\[(.*?)\]\]|\{\{\s*([A-Za-z0-9_]+)\s*\}\}`)
	phRe    = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_]+)\s*\}\}`)
)

type Vars map[string]string

type GenInput struct {
	JobID                     string
	Kind, Model, Prompt, Size string
	Duration                  int
	Mode                      string       // 生成模式 references | frames（已按「自动」规则确定）
	Ref                       *RefImage    // 参考图；首尾帧模式下是首帧
	Refs                      []*RefImage  // 全部参考图（第一张就是 Ref），只有一张时可以留空
	RefLast                   *RefImage    // 尾帧
	Extras                    []ExtraParam // 附加参数，渲染模板后合并进请求体
}

func (in GenInput) allRefs() []*RefImage {
	if len(in.Refs) > 0 {
		return in.Refs
	}
	if in.Ref != nil {
		return []*RefImage{in.Ref}
	}
	return nil
}

// setRefVars 填多张图用的变量：{{images}} 是网址或 data URL 的数组，$each 展开时用 refs 里的每一项。
func setRefVars(v Vars, refs []*RefImage, preview bool) {
	urls := make([]string, 0, len(refs))
	items := make([]Vars, 0, len(refs))
	for _, ref := range refs {
		item := Vars{}
		setImageVars(item, "item", ref, preview, "<参考图base64>")
		urls = append(urls, item["item"])
		items = append(items, item)
	}
	a, _ := json.Marshal(urls)
	b, _ := json.Marshal(items)
	v["images"], v["refs"] = string(a), string(b)
}

func buildVars(in GenInput, preview bool) Vars {
	// uuid 每次提交现生成；job_id 是本程序的任务编号（同一任务重试时不变）
	v := Vars{"prompt": in.Prompt, "model": in.Model, "size": in.Size, "uuid": newUUID(), "job_id": firstNonEmpty(in.JobID, "job0000")}
	w, h, aspect, res := parseSize(in.Size)
	if w > 0 && h > 0 {
		v["width"], v["height"] = strconv.Itoa(w), strconv.Itoa(h)
		v["size_star"] = fmt.Sprintf("%d*%d", w, h)
	}
	v["aspect_ratio"], v["resolution"], v["resolution_upper"] = aspect, res, strings.ToUpper(res)
	if in.Duration > 0 {
		v["duration"], v["duration_str"] = strconv.Itoa(in.Duration), strconv.Itoa(in.Duration)
	}
	setImageVars(v, "image", in.Ref, preview, "<参考图base64>")
	setImageVars(v, "image_last", in.RefLast, preview, "<尾帧base64>")
	refs := in.allRefs()
	if len(refs) > 0 {
		setRefVars(v, refs, preview)
	}

	// 按生成模式分好的图片变量：全能参考的参考图，首尾帧的首帧/尾帧
	v["mode"] = firstNonEmpty(in.Mode, "references")
	if v["mode"] == "frames" {
		v["frames_first"], v["frames_last"] = v["image"], v["image_last"]
	} else if len(refs) > 1 {
		v["references_images"] = v["images"]
	} else {
		v["references_image"] = v["image"]
	}
	return v
}

// setImageVars 填 {{prefix}} {{prefix_b64}} {{prefix_mime}} {{prefix_url_or_b64}} 四个变量。
func setImageVars(v Vars, prefix string, ref *RefImage, preview bool, placeholder string) {
	if ref == nil {
		return
	}
	mime := firstNonEmpty(ref.Mime, "image/png")
	b64 := ""
	if len(ref.Data) > 0 {
		b64 = base64.StdEncoding.EncodeToString(ref.Data)
	} else if preview {
		b64 = placeholder
	}
	v[prefix+"_mime"], v[prefix+"_b64"] = mime, b64
	if ref.URL != "" {
		v[prefix], v[prefix+"_url_or_b64"] = ref.URL, ref.URL
	} else {
		v[prefix], v[prefix+"_url_or_b64"] = "data:"+mime+";base64,"+b64, b64
	}
}

var (
	sizeRe  = regexp.MustCompile(`^(\d+)\s*[xX×*]\s*(\d+)$`)
	ratioRe = regexp.MustCompile(`^(\d+)\s*:\s*(\d+)$`)
)

// parseSize 从「尺寸」推出宽高、画面比例和分辨率档位，例如 1280x720 → 16:9、720p。
func parseSize(s string) (w, h int, aspect, res string) {
	s = strings.TrimSpace(s)
	if m := sizeRe.FindStringSubmatch(s); m != nil {
		w, _ = strconv.Atoi(m[1])
		h, _ = strconv.Atoi(m[2])
		if w > 0 && h > 0 {
			aspect, res = nearestRatio(w, h), resolutionOf(w, h)
		}
		return
	}
	if m := ratioRe.FindStringSubmatch(s); m != nil {
		return 0, 0, m[1] + ":" + m[2], ""
	}
	if l := strings.ToLower(s); strings.HasSuffix(l, "p") || l == "2k" || l == "4k" {
		res = l
	}
	return
}

func nearestRatio(w, h int) string {
	r := float64(w) / float64(h)
	for _, c := range [][2]int{{16, 9}, {9, 16}, {1, 1}, {4, 3}, {3, 4}, {3, 2}, {2, 3}, {21, 9}, {9, 21}, {5, 4}, {4, 5}} {
		cr := float64(c[0]) / float64(c[1])
		if math.Abs(r-cr)/cr < 0.02 {
			return fmt.Sprintf("%d:%d", c[0], c[1])
		}
	}
	a, b := w, h
	for b != 0 {
		a, b = b, a%b
	}
	return fmt.Sprintf("%d:%d", w/a, h/a)
}

func resolutionOf(w, h int) string {
	switch m := min(w, h); {
	case m >= 2160:
		return "4k"
	case m >= 1440:
		return "2k"
	default:
		return fmt.Sprintf("%dp", m)
	}
}

func unknownVars(tpl string) string {
	for _, m := range phRe.FindAllStringSubmatch(tpl, -1) {
		known := false
		for _, k := range templateVars {
			if k == m[1] {
				known = true
				break
			}
		}
		if !known {
			return m[1]
		}
	}
	return ""
}

// renderTemplate 把 JSON 模板渲染成要发送的值。
func renderTemplate(tpl string, vars Vars) (any, error) {
	if strings.TrimSpace(tpl) == "" {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(strings.NewReader(tpl))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("请求体模板不是合法的 JSON: %w", err)
	}
	if bad := unknownVars(tpl); bad != "" {
		return nil, fmt.Errorf("请求体模板里有未知变量 {{%s}}", bad)
	}
	out, absent := renderValue(v, vars)
	if absent {
		return map[string]any{}, nil
	}
	return out, nil
}

func renderValue(v any, vars Vars) (any, bool) {
	switch x := v.(type) {
	case string:
		return renderString(x, vars)
	case map[string]any:
		if cond, ok := x["$if"].(string); ok && vars[strings.Trim(cond, "{} ")] == "" {
			return nil, true
		}
		out := make(map[string]any, len(x))
		for k, val := range x {
			if k == "$if" {
				continue
			}
			if nv, absent := renderValue(val, vars); !absent {
				out[k] = nv
			}
		}
		if len(out) == 0 && len(x) > 0 {
			return nil, true
		}
		return out, false
	case []any:
		out := make([]any, 0, len(x))
		for _, el := range x {
			if m, ok := el.(map[string]any); ok && m["$each"] != nil {
				out = append(out, renderEach(m, vars)...)
				continue
			}
			if nv, absent := renderValue(el, vars); !absent {
				out = append(out, nv)
			}
		}
		return out, false
	}
	return v, false
}

// renderEach 展开数组里写了 "$each": "refs" 的对象：每张参考图生成一个元素，里面可以用 {{item}} 等变量。
func renderEach(tpl map[string]any, vars Vars) []any {
	name, _ := tpl["$each"].(string)
	var items []Vars
	if json.Unmarshal([]byte(vars[strings.Trim(name, "{} ")]), &items) != nil {
		return nil
	}
	body := maps.Clone(tpl)
	delete(body, "$each")
	var out []any
	for _, item := range items {
		merged := maps.Clone(vars)
		maps.Copy(merged, item)
		if nv, absent := renderValue(body, merged); !absent {
			out = append(out, nv)
		}
	}
	return out
}

// renderString 单遍扫描模板，替换进来的用户内容不会再被当成模板解析。
func renderString(s string, vars Vars) (any, bool) {
	if m := phRe.FindStringSubmatch(strings.TrimSpace(s)); m != nil && m[0] == strings.TrimSpace(s) {
		val := vars[m[1]]
		if val == "" {
			return nil, true
		}
		if numericVars[m[1]] {
			if n, err := strconv.Atoi(val); err == nil {
				return n, false
			}
		}
		if listVars[m[1]] {
			var list []any
			if json.Unmarshal([]byte(val), &list) == nil {
				return list, false
			}
		}
		return val, false
	}
	absent := false
	out := tokenRe.ReplaceAllStringFunc(s, func(tok string) string {
		m := tokenRe.FindStringSubmatch(tok)
		if m[2] != "" {
			val := vars[m[2]]
			if val == "" {
				absent = true
			}
			return val
		}
		missing := false
		inner := phRe.ReplaceAllStringFunc(m[1], func(p string) string {
			val := vars[phRe.FindStringSubmatch(p)[1]]
			if val == "" {
				missing = true
			}
			return val
		})
		if missing {
			return ""
		}
		return inner
	})
	if absent {
		return nil, true
	}
	return out, false
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// applyExtras 把附加参数写进已渲染的请求体。nested=true 时 a.b 写成嵌套对象（JSON），表单则按原名作字段。
// 值里可以用 {{uuid}}、{{job_id}} 等变量。
func applyExtras(obj map[string]any, extras []ExtraParam, nested bool, vars Vars) {
	for _, e := range extras {
		val := typedValue(e.Value)
		if strings.Contains(e.Value, "{{") {
			rendered, absent := renderString(e.Value, vars)
			if absent {
				continue
			}
			val = rendered
		}
		if !nested || !strings.Contains(e.Key, ".") {
			obj[e.Key] = val
			continue
		}
		keys := strings.Split(e.Key, ".")
		cur := obj
		for _, k := range keys[:len(keys)-1] {
			next, ok := cur[k].(map[string]any)
			if !ok {
				next = map[string]any{}
				cur[k] = next
			}
			cur = next
		}
		cur[keys[len(keys)-1]] = val
	}
}

var numberRe = regexp.MustCompile(`^-?\d+(\.\d+)?$`)

// typedValue：true/false/数字按原类型；"..." 强制字符串；其余为字符串。
func typedValue(s string) any {
	switch {
	case s == "true":
		return true
	case s == "false":
		return false
	case numberRe.MatchString(s):
		return json.Number(s)
	case len(s) >= 2 && strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`):
		return s[1 : len(s)-1]
	}
	return s
}

// renderPath 替换路径里的 {model}、{id}、{file_id}；没提供的占位符原样保留。
func renderPath(tpl string, repl map[string]string) string {
	for k, v := range repl {
		esc := strings.ReplaceAll(url.PathEscape(v), "%2F", "/")
		tpl = strings.ReplaceAll(tpl, "{"+k+"}", esc)
	}
	return tpl
}

func marshalJSON(v any, indent bool) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if indent {
		enc.SetIndent("", "  ")
	}
	_ = enc.Encode(v)
	return bytes.TrimRight(buf.Bytes(), "\n")
}

func toFieldString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case int:
		return strconv.Itoa(x)
	case bool:
		return strconv.FormatBool(x)
	case nil:
		return ""
	}
	return string(marshalJSON(v, false))
}
