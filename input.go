package main

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Row 是一条输入；留空的字段用表单上的默认值。
type Row struct {
	Kind, Prompt, Model, Size, Image string
	Refs                             []string // 第二张起的参考图（CSV 的 image 列用 | 分隔多张）
	Mode, LastImage                  string   // 生成模式（references/frames）和尾帧
	VideoPrompt                      string   // 图转视频：这一行的视频提示词
	Duration, Count                  int
}

// parseMode 认中英文写法，返回 references / frames；「自动」或认不出返回空。
func parseMode(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "references", "reference", "全能参考", "参考", "文生", "参考图":
		return "references"
	case "frames", "frame", "首尾帧", "首帧", "首尾":
		return "frames"
	}
	return ""
}

func parseLines(text string) []Row {
	var rows []Row
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rows = append(rows, Row{Prompt: line})
	}
	return rows
}

var csvAliases = map[string]string{
	"prompt": "prompt", "提示词": "prompt", "描述": "prompt",
	"type": "type", "kind": "type", "类型": "type",
	"model": "model", "模型": "model",
	"size": "size", "尺寸": "size", "分辨率": "size",
	"duration": "duration", "seconds": "duration", "时长": "duration",
	"image": "image", "ref": "image", "参考图": "image",
	"count": "count", "n": "count", "数量": "count",
	"mode": "mode", "模式": "mode", "生成模式": "mode",
	"last_image": "last_image", "image_last": "last_image", "尾帧": "last_image",
	"first_image": "image", "首帧": "image",
	"video_prompt": "video_prompt", "视频提示词": "video_prompt",
}

func parseCSV(data []byte) ([]Row, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(data) {
		return nil, errors.New("CSV 不是 UTF-8 编码。请在 Excel 中选择「另存为 → CSV UTF-8（逗号分隔）」后再导入")
	}
	rd := csv.NewReader(bytes.NewReader(data))
	rd.FieldsPerRecord = -1
	rd.LazyQuotes = true
	rd.TrimLeadingSpace = true
	recs, err := rd.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("CSV 解析失败: %w", err)
	}
	if len(recs) < 2 {
		return nil, errors.New("CSV 至少需要一行表头和一行数据")
	}

	cols := map[string]int{}
	for i, h := range recs[0] {
		if k, ok := csvAliases[strings.ToLower(strings.TrimSpace(h))]; ok {
			if _, dup := cols[k]; !dup {
				cols[k] = i
			}
		}
	}
	if _, ok := cols["prompt"]; !ok {
		return nil, errors.New("CSV 表头里找不到 prompt（或「提示词」）列")
	}
	get := func(rec []string, k string) string {
		i, ok := cols[k]
		if !ok || i >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[i])
	}

	var rows []Row
	for n, rec := range recs[1:] {
		line := n + 2
		row := Row{Prompt: get(rec, "prompt"), Model: get(rec, "model"), Size: get(rec, "size"), Image: get(rec, "image"),
			Mode: parseMode(get(rec, "mode")), LastImage: get(rec, "last_image"), VideoPrompt: get(rec, "video_prompt")}
		if row.Prompt == "" {
			continue
		}
		images := strings.Split(row.Image, "|")
		row.Image = strings.TrimSpace(images[0])
		for _, s := range images[1:] {
			if s = strings.TrimSpace(s); s != "" {
				row.Refs = append(row.Refs, s)
			}
		}
		switch t := strings.ToLower(get(rec, "type")); t {
		case "":
		case "image", "img", "图片", "图像":
			row.Kind = KindImage
		case "video", "视频":
			row.Kind = KindVideo
		default:
			return nil, fmt.Errorf("第 %d 行：无法识别的类型 %q（应为 image 或 video）", line, t)
		}
		if v := get(rec, "duration"); v != "" {
			d, err := strconv.Atoi(strings.TrimSuffix(strings.ToLower(v), "s"))
			if err != nil || d <= 0 {
				return nil, fmt.Errorf("第 %d 行：时长 %q 不是正整数", line, v)
			}
			row.Duration = d
		}
		if v := get(rec, "count"); v != "" {
			c, err := strconv.Atoi(v)
			if err != nil || c <= 0 {
				return nil, fmt.Errorf("第 %d 行：数量 %q 不是正整数", line, v)
			}
			row.Count = c
		}
		rows = append(rows, row)
	}
	return rows, nil
}
