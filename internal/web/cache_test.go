package web

import (
	"net/http"
	"testing"
)

func TestPageCacheKey(t *testing.T) {
	r, err := http.NewRequest(http.MethodGet, "/poems?utm_source=x&b=2&a=1&empty=&fbclid=z", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pageCacheKey(r), "/poems?a=1&b=2"; got != want {
		t.Fatalf("pageCacheKey=%q want %q", got, want)
	}

	r2, _ := http.NewRequest(http.MethodGet, "/poems?page=2&keyword=%E6%9C%88", nil)
	if got, want := pageCacheKey(r2), "/poems?keyword=%E6%9C%88&page=2"; got != want {
		t.Fatalf("参数未排序: %q want %q", got, want)
	}

	r3, _ := http.NewRequest(http.MethodGet, "/", nil)
	if got := pageCacheKey(r3); got != "/" {
		t.Fatalf("空查询串: %q", got)
	}
}
