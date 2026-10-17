package model

import (
	"net/url"
	"strings"
)

// Poet 诗人。库中没有独立诗人表，诗人由 poems 的 author/dynasty 聚合派生；
// Intro 取自内置诗人小传（data.Poets），未收录则为空。
type Poet struct {
	Name      string `json:"name"`
	Dynasty   string `json:"dynasty"`
	Intro     string `json:"intro"`
	PoemCount int    `json:"poem_count"` // 作品数（列表查询时填充）
}

// URL 前台诗人详情地址（以姓名作为标识）。
func (p Poet) URL() string { return "/poets/" + url.PathEscape(p.Name) }

// EditURL 后台编辑地址。
func (p Poet) EditURL() string { return "/admin/poets/" + url.PathEscape(p.Name) + "/edit" }

// DeleteURL 后台删除地址。
func (p Poet) DeleteURL() string { return "/admin/poets/" + url.PathEscape(p.Name) + "/delete" }

// Poem 诗词条目，对应 poems 表。
// Content 为逐行文本（每行一句，含标点）；Tags 为逗号分隔标签（库中为 JSON 数组）。
type Poem struct {
	ID            int64  `json:"id"`
	Title         string `json:"title"`
	Author        string `json:"author"`
	Dynasty       string `json:"dynasty"`
	Category      string `json:"category"`
	Content       string `json:"content"`
	Translation   string `json:"translation"`
	Annotation    string `json:"annotation"`
	Appreciation  string `json:"appreciation"`
	Tags          string `json:"tags"`
	Featured      bool   `json:"featured"`
	ViewCount     int64  `json:"view_count"`
	FavoriteCount int    `json:"favorite_count"`
	CommentCount  int    `json:"comment_count"`
	Status        string `json:"status"`
}

// AuthorURL 作者详情地址。
func (p Poem) AuthorURL() string { return "/poets/" + url.PathEscape(p.Author) }

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

// Brief 短摘要：优先取赏析/译文开头，缺失时退回正文前两句，按字符数截断。
func (p Poem) Brief(n int) string {
	s := strings.TrimSpace(p.Appreciation)
	if s == "" {
		s = strings.TrimSpace(p.Translation)
	}
	if s == "" {
		return p.Excerpt()
	}
	r := []rune(s)
	if n > 0 && len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
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
