package model

import "strings"

// Poet 诗人（或作品集合，如《诗经》、汉乐府）。
type Poet struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Dynasty   string `json:"dynasty"`
	Intro     string `json:"intro"`
	PoemCount int    `json:"poem_count"` // 作品数（列表查询时填充）
}

// Poem 诗词条目。Content 为逐行文本（每行一句，含标点）。
type Poem struct {
	ID       int64  `json:"id"`
	Title    string `json:"title"`
	PoetID   int64  `json:"poet_id"`
	PoetName string `json:"poet_name"`
	Dynasty  string `json:"dynasty"`
	Genre    string `json:"genre"`
	Content  string `json:"content"`
	Tags     string `json:"tags"`
	Note     string `json:"note"`
}

// Lines 按行拆分正文。
func (p Poem) Lines() []string {
	lines := strings.Split(p.Content, "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			out = append(out, strings.TrimSpace(l))
		}
	}
	return out
}

// Excerpt 摘要：前两句。
func (p Poem) Excerpt() string {
	lines := p.Lines()
	if len(lines) == 0 {
		return ""
	}
	if len(lines) >= 2 {
		return lines[0] + " " + lines[1]
	}
	return lines[0]
}

// TagList 标签拆分。
func (p Poem) TagList() []string {
	parts := strings.Split(p.Tags, ",")
	out := make([]string, 0, len(parts))
	for _, t := range parts {
		if strings.TrimSpace(t) != "" {
			out = append(out, strings.TrimSpace(t))
		}
	}
	return out
}
