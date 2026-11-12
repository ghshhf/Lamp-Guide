package gateway

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"zenui/internal/zen/chprov"
	"zenui/internal/zen/store"
)

type reviewQuotaRT func(*http.Request) (*http.Response, error)

func (f reviewQuotaRT) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestRegressionQuotaRefreshCoalesces(t *testing.T) {
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	st.Mutate(func(c *store.Config) {
		c.ChannelAccounts = map[string]chprov.Accounts{"cline": {{ID: "review-account", AccessToken: "review-dummy-token"}}}
	})
	s := &Server{Store: st}
	s.forgetQuota("", "")
	defer s.forgetQuota("", "")
	original := chprov.Client
	defer func() { chprov.Client = original }()
	var count atomic.Int32
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	chprov.Client = &http.Client{Transport: reviewQuotaRT(func(r *http.Request) (*http.Response, error) {
		count.Add(1)
		entered <- struct{}{}
		<-release
		return &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader("review mock response")), Header: make(http.Header), Request: r}, nil
	})}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.chanQuotas("cline") }()
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		close(release)
		wg.Wait()
		t.Fatal("no quota request")
	}
	select {
	case <-entered:
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	wg.Wait()
	if got := count.Load(); got != 1 {
		t.Fatalf("two concurrent cold-cache reads issued %d upstream requests; want 1 coalesced refresh", got)
	}
}

func TestQuotaInvalidationSeparatesInflightGeneration(t *testing.T) {
	s := &Server{}
	entered, release := make(chan struct{}), make(chan struct{})
	oldDone := make(chan struct{})
	go func() {
		s.cachedQuota("qoder|id:a|same", func() chprov.Quota { close(entered); <-release; return chprov.Quota{Never: true, Remaining: "old"} })
		close(oldDone)
	}()
	<-entered
	s.forgetQuota("qoder", "")
	next := make(chan chprov.Quota, 1)
	go func() {
		next <- s.cachedQuota("qoder|id:a|same", func() chprov.Quota { return chprov.Quota{Never: true, Remaining: "new"} })
	}()
	select {
	case q := <-next:
		if q.Remaining != "new" {
			t.Error("reused stale flight")
		}
	case <-time.After(time.Second):
		close(release)
		<-oldDone
		t.Fatal("new read waited on invalidated flight")
	}
	close(release)
	<-oldDone
	q := s.cachedQuota("qoder|id:a|same", func() chprov.Quota { t.Error("lost fresh cache"); return chprov.Quota{} })
	if q.Remaining != "new" {
		t.Fatal("old flight replaced refreshed cache")
	}
}

func TestFoldQuotasSkipsExpiredAccount(t *testing.T) {
	future := time.Now().Add(time.Hour).UnixMilli()
	q := foldQuotas(map[string]chprov.Quota{"expired": {Known: true, ExpiryMS: 1}, "active": {Known: true, ExpiryMS: future}})
	if !q.Known || q.ExpiryMS != future {
		t.Fatalf("expired account masked active one: %+v", q)
	}
}

// A funded account must not disappear from the aggregate chain because one
// 12-second quota read timed out. The last good reading carries the turn.
func TestCachedQuotaKeepsLastGoodReading(t *testing.T) {
	s := &Server{}
	good := chprov.Quota{Known: true, ExpiryMS: time.Now().Add(24 * time.Hour).UnixMilli()}
	calls := 0
	read := func() chprov.Quota {
		calls++
		if calls == 1 {
			return good
		}
		return chprov.Fail("临时超时")
	}
	const key = "test|funded-account"
	if got := s.cachedQuota(key, read); !got.OK() {
		t.Fatalf("第一次读数应可用: %+v", got)
	}
	s.quotaMu.Lock()
	ent := s.quotaCache[key]
	ent.at = time.Now().Add(-2 * chanQuotaTTL)
	s.quotaCache[key] = ent
	s.quotaMu.Unlock()

	got := s.cachedQuota(key, read)
	if !got.OK() || got.ExpiryMS != good.ExpiryMS {
		t.Fatalf("瞬时读失败后应沿用上次成功读数, got=%+v", got)
	}
	// A failure must not be pinned for the full window either.
	s.quotaMu.Lock()
	age := time.Since(s.quotaCache[key].at)
	s.quotaMu.Unlock()
	if age < chanQuotaTTL-quotaRetryDelay-2*time.Second {
		t.Fatalf("失败读数被钉在完整缓存窗口: age=%s", age)
	}
}
