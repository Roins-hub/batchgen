//go:build !windows

package main

import "fmt"

// 其它系统没有内置窗口，用浏览器打开页面，服务一直运行到进程结束。
func showError(text string) { fmt.Println(text) }

func focusRunning() bool { return false }

func runUI(url, _ string, _ func() int) {
	openBrowser(url)
	select {}
}
