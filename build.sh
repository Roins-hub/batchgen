#!/usr/bin/env bash
# 编译 Windows 版应用到 dist/。所有 Go 缓存都在 F:\DevTools（见 env.sh）。
set -euo pipefail
source /f/DevTools/env.sh
cd "$(dirname "$0")"

# 图标、版本信息、高分屏声明写进 exe。改了 winres/ 里的文件才需要重新生成。
if [ ! -f rsrc_windows_amd64.syso ] || [ -n "$(find winres -newer rsrc_windows_amd64.syso -type f)" ]; then
  go run github.com/tc-hib/go-winres@v0.3.3 make --in winres/winres.json --out rsrc --arch amd64
fi

go vet ./...
# windowsgui：不带黑色控制台窗口
VERSION=$(sed -n 's/.*"ProductVersion": "\(.*\)".*/\1/p' winres/winres.json)
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w -H=windowsgui -X main.releaseVersion=$VERSION ${EXTRA_LDFLAGS:-}" -o dist/BatchGen.new.exe .

# 程序正在运行时 Windows 不允许覆盖 exe，但允许改名：先把旧文件挪开，再放入新文件。
# 之后双击新的 BatchGen.exe，它会自动接管正在运行的旧版本。
rm -f dist/BatchGen.old.exe dist/BatchGen.exe~ 2>/dev/null || true  # 还在运行的删不掉，忽略
if [ -f dist/BatchGen.exe ] && ! mv -f dist/BatchGen.new.exe dist/BatchGen.exe 2>/dev/null; then
  mv dist/BatchGen.exe dist/BatchGen.old.exe
  mv dist/BatchGen.new.exe dist/BatchGen.exe
  echo "提示：旧版本正在运行，旧文件已改名为 dist/BatchGen.old.exe（关掉旧版本后可以删除）"
elif [ -f dist/BatchGen.new.exe ]; then
  mv dist/BatchGen.new.exe dist/BatchGen.exe
fi
cp examples/示例.csv dist/
echo "完成：dist/BatchGen.exe（$VERSION）"
