package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"shici/internal/model"
)

// newTestStore 用临时 SQLite 库启动（含内置种子数据）。
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestDraftVisibility(t *testing.T) {
	s := newTestStore(t)
	id, err := s.CreatePoem(model.Poem{
		Title: "草稿测试", Author: "测试者", Dynasty: "唐", Category: "古诗",
		Content: "第一句。\n第二句。", Status: "draft",
	})
	if err != nil {
		t.Fatalf("CreatePoem: %v", err)
	}

	if _, err := s.GetPoem(id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("前台 GetPoem 不应返回草稿, err=%v", err)
	}
	if _, err := s.GetPoemForAdmin(id); err != nil {
		t.Fatalf("后台应能读取草稿: %v", err)
	}

	pub := &Filter{Page: 1, PageSize: 20, Keyword: "草稿测试"}
	if list, _, _ := s.ListPoems(pub); len(list) != 0 {
		t.Fatalf("前台列表不应包含草稿: %+v", list)
	}

	admin := &Filter{Page: 1, PageSize: 20, Keyword: "草稿测试", AllStatus: true}
	list, total, err := s.ListPoems(admin)
	if err != nil || total != 1 || len(list) != 1 {
		t.Fatalf("后台列表应包含草稿: total=%d list=%v err=%v", total, list, err)
	}
}

func TestListPageClampAndWildcard(t *testing.T) {
	s := newTestStore(t)

	f := &Filter{Page: 99999, PageSize: 20}
	list, total, err := s.ListPoems(f)
	if err != nil {
		t.Fatalf("ListPoems: %v", err)
	}
	if f.Page != PageCount(total, 20) {
		t.Fatalf("越界页码未回落到最后一页: page=%d total=%d", f.Page, total)
	}
	if len(list) == 0 {
		t.Fatalf("最后一页不应为空")
	}

	// % 应作为字面量匹配，而不是匹配全部
	if _, n, _ := s.ListPoems(&Filter{Page: 1, PageSize: 20, Keyword: "%"}); n != 0 {
		t.Fatalf("通配符未被转义, total=%d", n)
	}
}

func TestListColumnsTruncateContent(t *testing.T) {
	s := newTestStore(t)
	long := strings.Repeat("长", 500)
	if _, err := s.CreatePoem(model.Poem{
		Title: "长文测试", Author: "测试者", Dynasty: "唐", Category: "古诗",
		Content: long, Translation: strings.Repeat("译", 300), Status: "published",
	}); err != nil {
		t.Fatalf("CreatePoem: %v", err)
	}
	list, _, err := s.ListPoems(&Filter{Page: 1, PageSize: 20, Keyword: "长文测试"})
	if err != nil || len(list) != 1 {
		t.Fatalf("ListPoems: %v %v", list, err)
	}
	if n := len([]rune(list[0].Content)); n > 200 {
		t.Fatalf("列表正文未裁剪: %d", n)
	}
	if list[0].Translation != "" {
		t.Fatalf("列表不应回传译文")
	}
	full, err := s.GetPoem(list[0].ID)
	if err != nil || len([]rune(full.Content)) != 500 || full.Translation == "" {
		t.Fatalf("详情页应返回完整内容: err=%v len=%d", err, len([]rune(full.Content)))
	}
}
