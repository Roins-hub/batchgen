//go:build !windows

package main

import "net/http"

var proxyFunc = http.ProxyFromEnvironment
