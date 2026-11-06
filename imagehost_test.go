package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTempHosts 把两家临时图床换成本地假服务，返回 uguu 收到的请求次数。
func fakeTempHosts(t *testing.T, uguu, litterbox http.HandlerFunc) *atomic.Int32 {
	t.Helper()
	hits := &atomic.Int32{}
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		uguu(w, r)
	}))
	b := httptest.NewServer(litterbox)
	oldUguu, oldLitter := uguuAPI, litterboxAPI
	uguuAPI, litterboxAPI = a.URL, b.URL
	t.Cleanup(func() {
		uguuAPI, litterboxAPI = oldUguu, oldLitter
		a.Close()
		b.Close()
	})
	return hits
}

func uguuOK(w http.ResponseWriter, _ *http.Request) {
	fmt.Fprint(w, `{"success":true,"files":[{"url":"https:\/\/d.uguu.se\/a.png"}]}`)
}

func litterOK(w http.ResponseWriter, _ *http.Request) {
	fmt.Fprint(w, "https://litter.catbox.moe/b.png")
}

func refused(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusPreconditionFailed)
	fmt.Fprint(w, "Invalid uploader")
}

func autoHost() ImageHost {
	h := ImageHost{}
	h.normalize()
	return h
}

func pixel(data string) *RefImage { return &RefImage{Data: []byte(data), Mime: "image/png"} }

func TestAutoHostTriesUguuFirst(t *testing.T) {
	fakeTempHosts(t, uguuOK, litterOK)
	link, err := NewUploader().Upload(context.Background(), autoHost(), pixel("a"))
	if err != nil || link != "https://d.uguu.se/a.png" {
		t.Fatalf("link=%s err=%v", link, err)
	}
}

func TestAutoHostFallsBackAndRemembers(t *testing.T) {
	hits := fakeTempHosts(t, refused, litterOK)
	u := NewUploader()
	link, err := u.Upload(context.Background(), autoHost(), pixel("a"))
	if err != nil || link != "https://litter.catbox.moe/b.png" {
		t.Fatalf("要换下一家: %s %v", link, err)
	}
	if _, err := u.Upload(context.Background(), autoHost(), pixel("b")); err != nil {
		t.Fatal(err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("记住能用的那家，不再试不通的: uguu 被请求 %d 次", got)
	}
}

func TestAutoHostReportsEveryFailure(t *testing.T) {
	fakeTempHosts(t, refused, refused)
	_, err := NewUploader().Upload(context.Background(), autoHost(), pixel("a"))
	if err == nil || !strings.Contains(err.Error(), "uguu") || !strings.Contains(err.Error(), "litterbox") {
		t.Fatalf("两家的原因都要说: %v", err)
	}
}

func TestUguuReportsFailureBody(t *testing.T) {
	fakeTempHosts(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"success":false,"description":"too big"}`)
	}, refused)
	if _, err := uploadUguu(context.Background(), pixel("a")); err == nil || !strings.Contains(err.Error(), "too big") {
		t.Fatalf("要带上图床的说明: %v", err)
	}
}

func TestTempLinksExpireBeforeTheHostDeletesThem(t *testing.T) {
	if got := tempHosts[0].ttl(autoHost()); got >= 3*time.Hour {
		t.Fatalf("uguu 只保留 3 小时: %s", got)
	}
	if got := tempHosts[1].ttl(autoHost()); got >= 24*time.Hour {
		t.Fatalf("litterbox 默认保留 24 小时: %s", got)
	}
}

func TestOldLitterboxSettingBecomesAuto(t *testing.T) {
	h := ImageHost{Mode: "litterbox"}
	h.normalize()
	if h.Mode != "auto" {
		t.Fatalf("旧配置要迁移: %s", h.Mode)
	}
	for _, mode := range []string{"s3", "off"} {
		kept := ImageHost{Mode: mode}
		kept.normalize()
		if kept.Mode != mode {
			t.Fatalf("用户的选择不能改: %s", kept.Mode)
		}
	}
}
