"""把 dist/BatchGen.exe 打成可以发给别人的 zip（先运行 build.sh）。

只收录下面列出的文件：dist/ 同时是本机的数据目录，里面的 config.json 含 API Key，绝不能打进去。
"""
import json
from pathlib import Path
import zipfile

ROOT = Path(__file__).resolve().parent
version = json.loads((ROOT / "winres" / "winres.json").read_text(encoding="utf-8"))
version = version["RT_VERSION"]["#1"]["0000"]["info"]["0804"]["ProductVersion"]

NOTE = f"""BatchGen 批量生成器 {version}

1. 把整个文件夹解压到你想放的位置（不要放在 Program Files 里，也不要直接在压缩包里运行）。
2. 双击 BatchGen.exe。第一次打开时 Windows 可能提示「已保护你的电脑」，点「更多信息」→「仍要运行」。
3. 在「设置」里填接口地址和 API Key，点「测试连接」，然后就可以批量生成了。

配置、任务记录和生成的文件都保存在 BatchGen.exe 旁边，搬动时连同文件夹一起搬。
程序会自动检查新版本，有更新时窗口顶部会提示，点「立即更新」即可，不用重新下载压缩包。
要求 Windows 10 或 11。Windows 11 可直接使用；窗口组件 WebView2 缺失时程序会改用浏览器打开。
示例.csv 是批量任务表格的示例。
"""

folder = f"BatchGen-{version}"
target = ROOT / "release" / f"{folder}-win-x64.zip"
target.parent.mkdir(exist_ok=True)
with zipfile.ZipFile(target, "w", zipfile.ZIP_DEFLATED) as archive:
    archive.write(ROOT / "dist" / "BatchGen.exe", f"{folder}/BatchGen.exe")
    archive.write(ROOT / "examples" / "示例.csv", f"{folder}/示例.csv")
    archive.writestr(f"{folder}/使用说明.txt", NOTE.replace("\n", "\r\n").encode("utf-8-sig"))
print("完成：", target.relative_to(ROOT))
