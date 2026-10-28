package store

import (
	"strings"
	"testing"
)

func TestPageCount(t *testing.T) {
	cases := []struct{ total, size, want int }{
		{0, 12, 1},
		{-1, 12, 1},
		{1, 12, 1},
		{12, 12, 1},
		{13, 12, 2},
		{120, 20, 6},
		{10, 0, 1},
	}
	for _, c := range cases {
		if got := PageCount(c.total, c.size); got != c.want {
			t.Errorf("PageCount(%d,%d)=%d want %d", c.total, c.size, got, c.want)
		}
	}
}

func TestLikeEscapesWildcards(t *testing.T) {
	if got, want := like("a%b_c!d"), "%a!%b!_c!!d%"; got != want {
		t.Fatalf("like()=%q want %q", got, want)
	}
}

func TestClausesDefaultsToPublished(t *testing.T) {
	s := &Store{}
	where, args, order, orderArgs := s.clauses(&Filter{})
	if !strings.Contains(where, publishedCond) {
		t.Fatalf("默认应只查已发布: %q", where)
	}
	if order != "p.id" || len(orderArgs) != 0 {
		t.Fatalf("默认排序应为 id: %q %v", order, orderArgs)
	}
	if len(args) != 0 {
		t.Fatalf("无条件时不应有参数: %v", args)
	}
}

func TestClausesAllStatusIncludesDrafts(t *testing.T) {
	s := &Store{}
	where, _, _, _ := s.clauses(&Filter{AllStatus: true, Page: 1, PageSize: 20})
	if !strings.Contains(where, "p.status <> 'archived'") {
		t.Fatalf("后台列表应包含草稿并排除归档: %q", where)
	}
}

func TestClausesKeywordFallbackEscapesLike(t *testing.T) {
	s := &Store{fulltext: false}
	where, args, _, _ := s.clauses(&Filter{Keyword: "月%", Page: 1, PageSize: 12})
	if !strings.Contains(where, "LIKE ? ESCAPE '!'") {
		t.Fatalf("LIKE 应带 ESCAPE: %q", where)
	}
	if len(args) != 4 {
		t.Fatalf("关键词兜底应有 4 个参数: %v", args)
	}
	for _, a := range args {
		if a != "%月!%%" {
			t.Fatalf("通配符未转义: %v", args)
		}
	}
}

func TestClausesFulltextRanks(t *testing.T) {
	s := &Store{fulltext: true}
	where, args, order, orderArgs := s.clauses(&Filter{Keyword: "明月", Page: 1, PageSize: 12})
	if !strings.Contains(where, "MATCH(") || !strings.Contains(where, "BOOLEAN MODE") {
		t.Fatalf("应走全文索引: %q", where)
	}
	if !strings.Contains(order, "NATURAL LANGUAGE MODE") {
		t.Fatalf("应按相关度排序: %q", order)
	}
	if len(args) != 1 || len(orderArgs) != 1 {
		t.Fatalf("全文检索参数数量不对: %v %v", args, orderArgs)
	}
}
