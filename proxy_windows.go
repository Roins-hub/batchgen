//go:build windows

package main

import (
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows/registry"
)

// proxyFunc 决定请求走哪个代理：先看环境变量，再看 Windows 系统代理。
// Go 默认只认环境变量，而代理软件通常只设置系统代理，不处理的话请求会绕过代理走直连。
var proxyFunc = func(req *http.Request) (*url.URL, error) {
	if u, err := http.ProxyFromEnvironment(req); u != nil || err != nil {
		return u, err
	}
	s := readSystemProxy()
	host := req.URL.Hostname()
	if s.server == "" || isLoopback(host) || proxyBypassed(host, s.override) {
		return nil, nil
	}
	return pickProxy(s.server, req.URL.Scheme), nil
}

type systemProxy struct {
	server, override string
	at               time.Time
}

var (
	proxyMu    sync.Mutex
	proxyCache systemProxy
)

// readSystemProxy 读注册表，缓存 30 秒：用户开关代理后很快生效，又不用每个请求都读。
func readSystemProxy() systemProxy {
	proxyMu.Lock()
	defer proxyMu.Unlock()
	if time.Since(proxyCache.at) < 30*time.Second {
		return proxyCache
	}
	proxyCache = systemProxy{at: time.Now()}
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if err != nil {
		return proxyCache
	}
	defer k.Close()
	if on, _, err := k.GetIntegerValue("ProxyEnable"); err != nil || on == 0 {
		return proxyCache
	}
	proxyCache.server, _, _ = k.GetStringValue("ProxyServer")
	proxyCache.override, _, _ = k.GetStringValue("ProxyOverride")
	return proxyCache
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// pickProxy 解析 ProxyServer：可能是 127.0.0.1:7897，也可能是 http=a:1;https=b:2;socks=c:3。
func pickProxy(server, scheme string) *url.URL {
	addr, socks := "", ""
	for _, part := range strings.Split(server, ";") {
		key, val, found := strings.Cut(strings.TrimSpace(part), "=")
		switch {
		case !found:
			addr = firstNonEmpty(addr, key)
		case strings.EqualFold(key, scheme):
			addr = val
		case strings.EqualFold(key, "socks"):
			socks = val
		}
	}
	if addr == "" && socks != "" {
		addr = "socks5://" + socks
	}
	if addr == "" {
		return nil
	}
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	u, err := url.Parse(addr)
	if err != nil {
		return nil
	}
	return u
}

// proxyBypassed 按 ProxyOverride 判断是否绕过代理：分号分隔，* 是通配符，<local> 表示不带点的主机名。
func proxyBypassed(host, override string) bool {
	host = strings.ToLower(host)
	for _, p := range strings.Split(override, ";") {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "<local>" {
			if !strings.Contains(host, ".") {
				return true
			}
		} else if ok, _ := path.Match(p, host); ok && p != "" {
			return true
		}
	}
	return false
}
