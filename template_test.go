package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

func render(t *testing.T, tpl string, in GenInput) string {
	t.Helper()
	v, err := renderTemplate(tpl, buildVars(in, false))
	if err != nil {
		t.Fatal(err)
	}
	return string(marshalJSON(v, false))
}

func TestRenderTemplateRules(t *testing.T) {
	tpl := `{
	  "prompt": "{{prompt}}[[ --ar {{aspect_ratio}}]]",
	  "duration": "{{duration}}",
	  "seconds": "{{duration_str}}",
	  "image": "{{image}}",
	  "content": [
	    {"type": "text", "text": "{{prompt}}"},
	    {"$if": "image", "type": "image_url", "image_url": {"url": "{{image}}"}}
	  ],
	  "inst": {"img": {"b64": "{{image_b64}}", "mime": "{{image_mime}}"}, "keep": 1}
	}`

	got := render(t, tpl, GenInput{Prompt: `说"你好"`, Size: "1280x720", Duration: 5})
	want := `{"content":[{"text":"说\"你好\"","type":"text"}],"duration":5,"inst":{"keep":1},"prompt":"说\"你好\" --ar 16:9","seconds":"5"}`
	if got != want {
		t.Errorf("no ref:\n got  %s\n want %s", got, want)
	}

	got = render(t, tpl, GenInput{Prompt: "p", Ref: &RefImage{Data: []byte("abc"), Mime: "image/png"}})
	for _, s := range []string{`"image":"data:image/png;base64,YWJj"`, `"image_url":{"url":"data:image/png;base64,YWJj"}`,
		`"img":{"b64":"YWJj","mime":"image/png"}`, `"prompt":"p"`} {
		if !strings.Contains(got, s) {
			t.Errorf("with ref: missing %s in %s", s, got)
		}
	}
	if strings.Contains(got, "duration") || strings.Contains(got, "--ar") {
		t.Errorf("empty vars should be dropped: %s", got)
	}
}

func TestUserTextIsNotReparsed(t *testing.T) {
	got := render(t, `{"p": "{{prompt}}"}`, GenInput{Prompt: "{{size}} [[x]]", Size: "1x1"})
	if got != `{"p":"{{size}} [[x]]"}` {
		t.Errorf("prompt was re-parsed: %s", got)
	}
}

func TestUnknownVar(t *testing.T) {
	if _, err := renderTemplate(`{"a": "{{promt}}"}`, Vars{}); err == nil {
		t.Error("expected unknown variable error")
	}
}

func TestParseSize(t *testing.T) {
	cases := map[string][2]string{
		"1280x720": {"16:9", "720p"}, "1024*1536": {"2:3", "1024p"}, "1920×1080": {"16:9", "1080p"},
		"9:16": {"9:16", ""}, "1080P": {"", "1080p"}, "2K": {"", "2k"}, "": {"", ""},
	}
	for in, want := range cases {
		_, _, a, r := parseSize(in)
		if a != want[0] || r != want[1] {
			t.Errorf("parseSize(%q) = %q,%q want %q,%q", in, a, r, want[0], want[1])
		}
	}
}

func TestLookupAndOutputs(t *testing.T) {
	v, _ := parseJSON([]byte(`{"data":{"task_status":"SUCCEED","task_result":{"videos":[{"url":"https://x/a.mp4"},{"url":"https://x/b.mp4"}]}},"n":3}`))
	if got := lookupAll(v, "missing|data.task_result.videos.*.url"); len(got) != 2 || got[1] != "https://x/b.mp4" {
		t.Errorf("lookupAll = %v", got)
	}
	if got := lookupFirst(v, "data.task_result.videos[0].url"); got != "https://x/a.mp4" {
		t.Errorf("bracket index = %q", got)
	}
	if matchState(lookupFirst(v, "data.task_status"), "succeed", "failed") != stateSuccess {
		t.Error("state should match case-insensitively")
	}
	if matchState("true", "true", "") != stateSuccess || matchState("", "true", "") != stateRunning {
		t.Error("done-flag style status")
	}

	md := "图好了\n![img](https://cdn/x.png) 参考 https://example.com/doc"
	if outs := mediaFromString(md); len(outs) != 1 || outs[0].URL != "https://cdn/x.png" {
		t.Errorf("markdown image extraction = %+v", outs)
	}
	if outs := mediaFromString(strings.Repeat("QUJD", 20)); len(outs) != 1 || outs[0].B64 == "" {
		t.Errorf("raw base64 = %+v", outs)
	}
}

func TestInnermostMessage(t *testing.T) {
	body := `{"code":"fail_to_fetch_task","message":"{\"error\":{\"code\":\"invalid_params\",\"message\":\"mode 为必填项（references/frames）\"}}","data":null}`
	if got := innermostMessage(body); got != "mode 为必填项（references/frames）" {
		t.Errorf("innermostMessage = %q", got)
	}
	if got := innermostMessage("plain text error"); got != "plain text error" {
		t.Errorf("plain = %q", got)
	}
	if got := (&APIError{Status: 400, Body: body}).Error(); !strings.Contains(got, "HTTP 400") || !strings.HasSuffix(got, "mode 为必填项（references/frames）") {
		t.Errorf("APIError = %q", got)
	}
}

func TestDetectMissingParam(t *testing.T) {
	cases := []struct {
		msg   string
		param string
		opts  string
	}{
		{"mode 为必填项（references/frames）", "mode", "references,frames"},
		{"参数 resolution 不能为空", "resolution", ""},
		{"缺少参数：aspect_ratio", "aspect_ratio", ""},
		{"Missing required parameter: 'quality'", "quality", ""},
		{`"style" must be one of [vivid, natural]`, "style", "vivid,natural"},
		{"duration is required", "duration", ""},
		{"Field 'seconds' is required", "seconds", ""},
		{"按秒计费模型必须提供时长（duration）", "duration", ""},
		{"请指定画面比例(aspect_ratio)", "aspect_ratio", ""},
		{"duration is missing", "duration", ""},
		{"You must provide seconds for this model", "seconds", ""},
		{"内容审核未通过", "", ""},
		{"prompt 字符数不能少于 10", "", ""},
	}
	for _, c := range cases {
		h := detectMissingParam(c.msg)
		got, opts := "", ""
		if h != nil {
			got, opts = h.Param, strings.Join(h.Options, ",")
		}
		if got != c.param || opts != c.opts {
			t.Errorf("%q → %q [%s], want %q [%s]", c.msg, got, opts, c.param, c.opts)
		}
	}
}

func TestExtrasMerge(t *testing.T) {
	params := normExtraParams([]ExtraParam{
		{Kind: "video", Key: "watermark", Value: "false"},
		{Kind: "video", Model: "sd-2", Key: "mode", Value: "references"},
		{Kind: "video", Model: "sd-2", Key: "parameters.fps", Value: "24"},
		{Kind: "video", Model: "other", Key: "mode", Value: "frames"},
		{Kind: "image", Key: "quality", Value: "high"},
		{Kind: "video", Model: "sd-2", Key: "mode", Value: `"references"`}, // 覆盖前一条
	})
	spec, _ := presetByID("sora")
	in := GenInput{Kind: "video", Model: "sd-2", Prompt: "p", Extras: extrasFor(params, "video", "sd-2")}
	p, err := prepare(&spec, in, false)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"mode":"references","model":"sd-2","parameters":{"fps":24},"prompt":"p","watermark":false}`
	if string(p.Body) != want {
		t.Errorf("body = %s\nwant   %s", p.Body, want)
	}
}

func TestJobModeAndAutoFix(t *testing.T) {
	rule := []ExtraParam{{Kind: "video", Model: "sd", Key: "mode", Value: "references"}}
	val := func(ex []ExtraParam) string {
		for _, e := range ex {
			if e.Key == "mode" {
				return e.Value
			}
		}
		return "(无)"
	}
	cases := []struct {
		name  string
		job   Job
		rules []ExtraParam
		want  string
	}{
		{"自动+无规则：不发 mode", Job{Kind: "video"}, nil, "(无)"},
		{"自动+有规则+无尾帧", Job{Kind: "video"}, rule, "references"},
		{"自动+有规则+有尾帧", Job{Kind: "video", Image: "a.png", LastImage: "b.png"}, rule, "frames"},
		{"明确选首尾帧覆盖规则", Job{Kind: "video", Mode: "frames"}, rule, "frames"},
		{"明确选全能参考+无规则", Job{Kind: "video", Mode: "references"}, nil, "references"},
	}
	for _, c := range cases {
		if got := val(withJobMode(c.rules, c.job)); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}

	h := &FixHint{Param: "mode", Options: []string{"references", "frames"}}
	if v := chooseFixValue(Job{}, h); v != "references" {
		t.Errorf("auto fix (text) = %s", v)
	}
	if v := chooseFixValue(Job{LastImage: "x"}, h); v != "frames" {
		t.Errorf("auto fix (last frame) = %s", v)
	}
	if v := chooseFixValue(Job{Mode: "references"}, &FixHint{Param: "quality", Options: []string{"high", "low"}}); v != "high" {
		t.Errorf("other param = %s", v)
	}
	if v := chooseFixValue(Job{Mode: "references"}, &FixHint{Param: "resolution"}); v != "" {
		t.Errorf("no options, not mode = %q", v)
	}
}

func TestIDParamsGetUUID(t *testing.T) {
	for _, p := range []string{"client_task_id", "request_id", "trace_id", "external_task_id", "nonce", "idempotency_key", "uuid", "id"} {
		if v := chooseFixValue(Job{}, &FixHint{Param: p}); v != "{{uuid}}" {
			t.Errorf("%s → %q, want {{uuid}}", p, v)
		}
	}
	for _, p := range []string{"resolution", "prompt_id_list", "width"} {
		if v := chooseFixValue(Job{}, &FixHint{Param: p}); v != "" {
			t.Errorf("%s → %q, want empty (ask user)", p, v)
		}
	}
	if h := detectMissingParam("client_task_id 为必填项"); h == nil || h.Param != "client_task_id" {
		t.Errorf("detect client_task_id = %+v", h)
	}

	spec, _ := presetByID("sora")
	in := GenInput{JobID: "abc123", Kind: "video", Model: "m", Prompt: "p",
		Extras: []ExtraParam{{Kind: "video", Key: "client_task_id", Value: "{{uuid}}"}, {Kind: "video", Key: "biz", Value: "bg-{{job_id}}"}}}
	ids := map[string]bool{}
	uuidRe := regexp.MustCompile(`"client_task_id":"[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}"`)
	for range 3 {
		p, err := prepare(&spec, in, false)
		if err != nil {
			t.Fatal(err)
		}
		body := string(p.Body)
		m := uuidRe.FindString(body)
		if m == "" || !strings.Contains(body, `"biz":"bg-abc123"`) {
			t.Fatalf("body = %s", body)
		}
		ids[m] = true
	}
	if len(ids) != 3 {
		t.Errorf("uuid should differ per submit, got %d distinct", len(ids))
	}
}

func TestSemanticFix(t *testing.T) {
	j := Job{Kind: "video", Model: "m", Prompt: "一只橘猫在窗台上伸懒腰", Size: "1280x720", Duration: 4}
	for param, want := range map[string]string{"duration": "{{duration}}", "seconds": "{{duration}}", "aspect_ratio": "{{aspect_ratio}}", "resolution": "{{resolution}}"} {
		if got := chooseFixValue(j, &FixHint{Param: param}); got != want {
			t.Errorf("%s → %q want %q", param, got, want)
		}
	}
	// 任务本身没有时长时不乱填，交给用户
	if got := chooseFixValue(Job{}, &FixHint{Param: "duration"}); got != "" {
		t.Errorf("no duration → %q", got)
	}
	// 记住的规则 duration={{duration}} 按每个任务自己的时长、以数字发送
	spec, _ := presetByID("sora")
	in := GenInput{Kind: "video", Model: "m", Prompt: "p", Duration: 7,
		Extras: []ExtraParam{{Kind: "video", Key: "duration", Value: "{{duration}}"}}}
	p, err := prepare(&spec, in, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(p.Body), `"duration":7`) {
		t.Errorf("body = %s", p.Body)
	}
}

func TestSeedanceJSONPreset(t *testing.T) {
	spec, _ := presetByID("openai_video_seedance")
	first := &RefImage{URL: "https://x/first.png", Mime: "image/png"}
	last := &RefImage{URL: "https://x/last.png", Mime: "image/png"}
	cases := []struct {
		name string
		in   GenInput
		want string
	}{
		{"文生", GenInput{Mode: "references"},
			`{"client_task_id":"U","duration":4,"mode":"references","model":"m","prompt":"p"}`},
		{"参考图", GenInput{Mode: "references", Ref: first},
			`{"client_task_id":"U","duration":4,"mode":"references","model":"m","prompt":"p","references":{"image":"https://x/first.png"}}`},
		{"首尾帧", GenInput{Mode: "frames", Ref: first, RefLast: last},
			`{"client_task_id":"U","duration":4,"frames":{"first_frame":"https://x/first.png","last_frame":"https://x/last.png"},"mode":"frames","model":"m","prompt":"p"}`},
	}
	uuidRe := regexp.MustCompile(`"client_task_id":"[^"]+"`)
	for _, c := range cases {
		c.in.Kind, c.in.Model, c.in.Prompt, c.in.Duration = "video", "m", "p", 4
		p, err := prepare(&spec, c.in, false)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := uuidRe.ReplaceAllString(string(p.Body), `"client_task_id":"U"`); got != c.want {
			t.Errorf("%s:\n got  %s\n want %s", c.name, got, c.want)
		}
	}
	if !strings.Contains((&APIError{Status: 400, Body: `{"message":"JSON body required"}`}).Error(), "Seedance 扩展") {
		t.Error("JSON body required should suggest the JSON preset")
	}
}

func TestNeedsImageURL(t *testing.T) {
	yes := []string{"参考图片 URL 格式不正确", "首帧图片链接无效", "image url is invalid", "Invalid image URL", "only http(s) url supported for image: invalid url", "base64 图片不支持", "仅支持图片 URL"}
	no := []string{"mode 为必填项（references/frames）", "prompt 字符数不能少于 10", "模型 xx 不支持 frames 模式", "回调 url 格式不正确，请检查 callback"}
	for _, m := range yes {
		if !needsImageURL(&APIError{Status: 400, Body: `{"message":"` + m + `"}`}) {
			t.Errorf("should need URL: %q", m)
		}
	}
	for _, m := range no[:3] {
		if needsImageURL(&APIError{Status: 400, Body: `{"message":"` + m + `"}`}) {
			t.Errorf("should not need URL: %q", m)
		}
	}
}

func TestLastFrameRequests(t *testing.T) {
	first := &RefImage{URL: "https://x/first.png", Mime: "image/png"}
	last := &RefImage{URL: "https://x/last.png", Mime: "image/png"}
	sd, _ := presetByID("seedance")
	p, err := prepare(&sd, GenInput{Kind: "video", Model: "m", Prompt: "p", Ref: first, RefLast: last}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Preview, `"role": "last_frame"`) || !strings.Contains(p.Preview, "last.png") {
		t.Errorf("seedance last frame missing:\n%s", p.Preview)
	}
	sora, _ := presetByID("sora")
	p, err = prepare(&sora, GenInput{Kind: "video", Model: "m", Prompt: "p",
		Ref: &RefImage{Data: []byte("a"), Mime: "image/png"}, RefLast: &RefImage{Data: []byte("b"), Mime: "image/png"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(p.Body), `name="input_reference"`) != 2 {
		t.Errorf("sora form should upload 2 files:\n%s", p.Preview)
	}
	if _, err := prepare(&sora, GenInput{Kind: "video", Prompt: "p", RefLast: last}, true); err == nil {
		t.Error("last frame without first frame should fail")
	}
}

func TestKlingJWT(t *testing.T) {
	tok := klingJWT("ak123", "secret")
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt parts = %d", len(parts))
	}
	mac := hmac.New(sha256.New, []byte("secret"))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if base64.RawURLEncoding.EncodeToString(mac.Sum(nil)) != parts[2] {
		t.Error("bad signature")
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil || claims["iss"] != "ak123" {
		t.Errorf("claims = %s", payload)
	}
}

func TestLegacyConfigMigration(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.json"
	old := `{"base_url":"http://1.2.3.4:3000/v1","api_keys":["k"],"video_api":"sora","paths":{"video_submit":"/x"}}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	cs, err := LoadConfig(path, dir)
	if err != nil {
		t.Fatal(err)
	}
	c := cs.Get()
	if c.BaseURL != "http://1.2.3.4:3000" || c.VideoSpec.Preset != "sora" || c.ImageSpec.Preset != "openai_images" {
		t.Errorf("migrated config = base %q video %q image %q", c.BaseURL, c.VideoSpec.Preset, c.ImageSpec.Preset)
	}
}

func TestPresetsValid(t *testing.T) {
	for _, p := range presets {
		s, _ := presetByID(p.ID)
		if err := s.validate(p.Name); err != nil {
			t.Errorf("preset %s: %v", p.ID, err)
		}
		for _, ref := range []*RefImage{nil, {URL: "https://x/r.png", Mime: "image/png"}} {
			if _, err := prepare(&s, GenInput{Kind: p.Kind, Model: "m", Prompt: "p", Size: "1280x720", Duration: 5, Ref: ref}, true); err != nil &&
				!(ref != nil && p.ID == "wan_image") {
				t.Errorf("preset %s (ref=%v): %v", p.ID, ref != nil, err)
			}
		}
	}
}
