package web

import (
	"strconv"
	"sync"
	"time"

	"github.com/kataras/iris/v12"
)

// 开放 API 限流：全部接口合计 20 QPS（令牌桶，突发容量 = 1 秒的配额）。
// 公益站点为单实例部署，进程内计数即可；若横向扩容多实例，
// 需把计数挪到共享层（Redis）或在反向代理（Nginx/Caddy）侧限流。
const (
	apiQPS   = 20
	apiBurst = 20
)

// tokenBucket 令牌桶：按时间匀速补充令牌，取到令牌即放行，取不到即限流。
type tokenBucket struct {
	mu     sync.Mutex
	tokens float64
	rate   float64 // 每秒补充的令牌数
	burst  float64 // 桶容量（可容忍的瞬时突发）
	last   time.Time
}

func newTokenBucket(rate, burst float64) *tokenBucket {
	return &tokenBucket{tokens: burst, rate: rate, burst: burst, last: time.Now()}
}

// allow 取一个令牌，无可用令牌时返回 false。
func (b *tokenBucket) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	if d := now.Sub(b.last).Seconds(); d > 0 {
		b.tokens += d * b.rate
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// RateLimit 开放 API 限流中间件：超过配额返回 429（JSON），并给出 Retry-After。
// 返回的实例可挂在多个路由上共享同一个桶（即这些路由合计受限）。
func RateLimit() iris.Handler {
	b := newTokenBucket(apiQPS, apiBurst)
	return func(ctx iris.Context) {
		if !b.allow() {
			ctx.StatusCode(iris.StatusTooManyRequests)
			ctx.Header("Retry-After", "1")
			ctx.Header("X-RateLimit-Limit", strconv.Itoa(apiQPS))
			ctx.JSON(iris.Map{"error": "请求过于频繁，请稍后再试"})
			ctx.StopExecution()
			return
		}
		ctx.Next()
	}
}
