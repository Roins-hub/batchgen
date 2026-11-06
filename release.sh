#!/usr/bin/env bash
# 发布新版本：编译、打包、签名并上传，已安装的程序会自动收到更新。
# 用法：先改 winres/winres.json 里的版本号，再运行 bash release.sh "更新内容"
set -euo pipefail
[ $# -eq 1 ] || { echo "用法：bash release.sh \"这个版本的更新内容\"" >&2; exit 1; }
cd "$(dirname "$0")"
# env.sh 会改 APPDATA，gh 要用原来的目录才找得到登录信息。
export GH_CONFIG_DIR="${GH_CONFIG_DIR:-$APPDATA/GitHub CLI}"
source /f/DevTools/env.sh
go test ./...
bash build.sh
python -I package.py
bash installer/build.sh
# gh 走本机代理时常断，直连更稳
HTTP_PROXY= HTTPS_PROXY= ALL_PROXY= http_proxy= https_proxy= all_proxy= go run ./tools/release publish -notes "$1"
