#!/usr/bin/env bash
# 把 dist/BatchGen.exe 做成安装包 release/BatchGen-<版本>-Setup.exe（先运行 build.sh）。
# Inno Setup 和 WebView2 引导程序都放在 F:\DevTools，见 README。
# 额外参数原样传给 ISCC，例如测试时用 --define=ForceWebView2。
set -euo pipefail
source /f/DevTools/env.sh
cd "$(dirname "$0")/.."

ISCC="${ISCC:-/f/DevTools/tools/InnoSetup/ISCC.exe}"
WEBVIEW2_SETUP="${WEBVIEW2_SETUP:-F:\\DevTools\\dl\\wv2\\MicrosoftEdgeWebview2Setup.exe}"
VERSION=$(sed -n 's/.*"ProductVersion": "\(.*\)".*/\1/p' winres/winres.json)

[ -x "$ISCC" ] || { echo "找不到 Inno Setup：$ISCC" >&2; exit 1; }
[ -f "$WEBVIEW2_SETUP" ] || { echo "找不到 WebView2 引导程序：$WEBVIEW2_SETUP" >&2; exit 1; }

"$ISCC" --quiet-progress "--define=AppVersion=$VERSION" "--define=WebView2Setup=$WEBVIEW2_SETUP" "$@" installer/BatchGen.iss
echo "完成：release/BatchGen-$VERSION-Setup.exe"
