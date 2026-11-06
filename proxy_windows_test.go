//go:build windows

package main

import (
	"net/url"
	"testing"
)

func TestProxyParsing(t *testing.T) {
	cases := []struct{ server, scheme, want string }{
		{"127.0.0.1:7897", "https", "http://127.0.0.1:7897"},
		{"http=a:1;https=b:2;socks=c:3", "https", "http://b:2"},
		{"http=a:1;https=b:2", "http", "http://a:1"},
		{"socks=c:3", "https", "socks5://c:3"},
		{"http://p:8080", "https", "http://p:8080"},
	}
	for _, c := range cases {
		var got *url.URL
		if got = pickProxy(c.server, c.scheme); got == nil || got.String() != c.want {
			t.Errorf("pickProxy(%q, %q) = %v，应为 %s", c.server, c.scheme, got, c.want)
		}
	}
	for host, want := range map[string]bool{"192.168.1.5": true, "myhost": true, "api.example.com": false, "api.corp.local": true} {
		if got := proxyBypassed(host, "localhost;127.*;192.168.*;*.corp.local;<local>"); got != want {
			t.Errorf("proxyBypassed(%q) = %v，应为 %v", host, got, want)
		}
	}
	if !isLoopback("127.0.0.1") || !isLoopback("localhost") || isLoopback("api.example.com") {
		t.Error("isLoopback 判断不对")
	}
}
