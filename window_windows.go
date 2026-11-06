//go:build windows

package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
	"github.com/jchv/go-webview2/webviewloader"
	"golang.org/x/sys/windows"
)

// iconID 对应 winres/winres.json 里的 RT_GROUP_ICON "#1"。
const iconID = 1

// instanceMutex 表示程序正在运行，安装包靠它判断要不要先关程序，和 installer/BatchGen.iss 的 AppMutex 一致。
// 只作标记，不用它防多开：句柄一直持有到进程退出，接管时新旧进程可以短暂同时持有。
const instanceMutex = "BatchGenRunning"

const (
	wmClose         = 0x0010
	gwlpWndProc     = ^uintptr(3) // GWLP_WNDPROC = -4
	smCxScreen      = 0
	smCyScreen      = 1
	swRestore       = 9
	mbOK            = 0x00000000
	mbYesNo         = 0x00000004
	mbIconError     = 0x00000010
	mbIconWarning   = 0x00000030
	mbIconInfo      = 0x00000040
	mbDefButton2    = 0x00000100
	mbSetForeground = 0x00010000
	idYes           = 6
)

var (
	user32             = syscall.NewLazyDLL("user32.dll")
	procMessageBoxW    = user32.NewProc("MessageBoxW")
	procFindWindowW    = user32.NewProc("FindWindowW")
	procSetForeground  = user32.NewProc("SetForegroundWindow")
	procShowWindow     = user32.NewProc("ShowWindow")
	procIsIconic       = user32.NewProc("IsIconic")
	procSetWindowLong  = user32.NewProc("SetWindowLongPtrW")
	procCallWindowProc = user32.NewProc("CallWindowProcW")
	procGetDpiForSys   = user32.NewProc("GetDpiForSystem")
	procSystemMetrics  = user32.NewProc("GetSystemMetrics")
)

// 窗口的消息循环必须留在创建窗口的线程上，main 协程从一开始就绑定主线程。
func init() { runtime.LockOSThread() }

func utf16Ptr(s string) *uint16 {
	p, _ := syscall.UTF16PtrFromString(s)
	return p
}

func messageBox(hwnd uintptr, text string, flags uintptr) uintptr {
	r, _, _ := procMessageBoxW.Call(hwnd, uintptr(unsafe.Pointer(utf16Ptr(text))),
		uintptr(unsafe.Pointer(utf16Ptr(appName))), flags|mbSetForeground)
	return r
}

func showError(text string) { messageBox(0, text, mbOK|mbIconError) }

// focusRunning 把已经打开的窗口切到前台；没有窗口（如正在用浏览器模式）时返回 false。
func focusRunning() bool {
	hwnd, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(utf16Ptr("webview"))), uintptr(unsafe.Pointer(utf16Ptr(appName))))
	if hwnd == 0 {
		return false
	}
	if iconic, _, _ := procIsIconic.Call(hwnd); iconic != 0 {
		procShowWindow.Call(hwnd, swRestore)
	}
	procSetForeground.Call(hwnd)
	return true
}

// confirmExit 在还有任务生成时询问是否退出；没有任务时直接同意。
func confirmExit(hwnd uintptr, active func() int) bool {
	n := active()
	if n == 0 {
		return true
	}
	text := fmt.Sprintf("还有 %d 个任务正在生成，确定要退出吗？\n\n已提交的任务下次打开时会继续查询结果；正在提交中的任务可能失败，需要重试。", n)
	return messageBox(hwnd, text, mbYesNo|mbIconWarning|mbDefButton2) == idYes
}

// scaled 按系统缩放比例换算窗口尺寸，并保证不超过屏幕。
func scaled(size, screenMetric uintptr) uint {
	dpi, _, _ := procGetDpiForSys.Call()
	if dpi == 0 {
		dpi = 96
	}
	screen, _, _ := procSystemMetrics.Call(screenMetric)
	return min(uint(size*dpi/96), uint(screen))
}

// hasWebView 检查系统是否装有 WebView2 运行时（Windows 11 自带）。
func hasWebView() bool {
	v, err := webviewloader.GetInstalledVersion()
	return err == nil && v != ""
}

// runUI 打开应用窗口并一直阻塞到用户退出。
func runUI(url, dir string, active func() int) {
	if name, err := windows.UTF16PtrFromString(instanceMutex); err == nil {
		windows.CreateMutex(nil, false, name) // 已存在时也返回有效句柄，这里不用关心结果
	}
	dataPath := filepath.Join(dir, "webview-data")
	if !hasWebView() || os.MkdirAll(dataPath, 0o755) != nil {
		runInBrowser(url, active)
		return
	}
	// 窗口组件出错时只写日志；没有控制台，留个文件才查得到原因。
	if f, err := os.OpenFile(filepath.Join(dir, "batchgen.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		log.SetOutput(f)
	}

	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:    true, // 打开后输入框才有右键菜单（复制、粘贴）
		DataPath: dataPath,
		WindowOptions: webview2.WindowOptions{
			Title:  appName,
			Width:  scaled(1320, smCxScreen),
			Height: scaled(900, smCyScreen),
			IconId: iconID,
			Center: true,
		},
	})
	if w == nil {
		runInBrowser(url, active)
		return
	}
	defer w.Destroy()
	w.SetSize(int(scaled(900, smCxScreen)), int(scaled(620, smCyScreen)), webview2.HintMin)

	// 接管窗口过程，在关闭前询问：窗口组件收到 WM_CLOSE 会直接销毁窗口。
	hwnd := uintptr(w.Window())
	var oldProc uintptr
	proc := syscall.NewCallback(func(h, msg, wp, lp uintptr) uintptr {
		if msg == wmClose && !confirmExit(h, active) {
			return 0
		}
		r, _, _ := procCallWindowProc.Call(oldProc, h, msg, wp, lp)
		return r
	})
	oldProc, _, _ = procSetWindowLong.Call(hwnd, gwlpWndProc, proc)

	w.Navigate(url)
	w.Run()
}

// runInBrowser 是没有 WebView2 时的退路：用浏览器打开，用一个对话框代替窗口来退出程序。
func runInBrowser(url string, active func() int) {
	openBrowser(url)
	for {
		messageBox(0, "系统没有安装 WebView2，已改用浏览器打开："+url+"\n\n程序正在后台运行，点「确定」退出程序。", mbOK|mbIconInfo)
		if confirmExit(0, active) {
			return
		}
	}
}
