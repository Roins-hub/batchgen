; BatchGen 安装包。用 installer/build.sh 编译，不要直接打开编译：版本号和引导程序路径由它传入。
;
; 安装到用户目录，不需要管理员权限，自动更新才能替换 exe（Program Files 里写不进去）。
; 程序的数据跟着 exe 走，卸载时只删安装的文件和缓存，用户的图片、视频和配置都保留。

#ifndef AppVersion
  #error 缺少 AppVersion，请运行 installer/build.sh
#endif
#ifndef WebView2Setup
  #error 缺少 WebView2Setup，请运行 installer/build.sh
#endif

#define AppTitle "BatchGen 批量生成器"

[Setup]
; AppId 决定升级时认不认得出旧版本，不能改
AppId={{8D2B7C1E-4F3A-4E6B-9A57-2C6E1F0B7D31}
AppName={#AppTitle}
AppVersion={#AppVersion}
AppVerName={#AppTitle} {#AppVersion}
AppPublisher=BatchGen
VersionInfoVersion={#AppVersion}
DefaultDirName={autopf}\BatchGen
DefaultGroupName={#AppTitle}
DisableProgramGroupPage=yes
DisableDirPage=no
UsePreviousAppDir=yes
PrivilegesRequired=lowest
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
MinVersion=10.0
; 和 window_windows.go 的 instanceMutex 一致：安装和卸载时都能发现程序正在运行
AppMutex=BatchGenRunning
RestartApplications=no
OutputDir=..\release
OutputBaseFilename=BatchGen-{#AppVersion}-Setup
SetupIconFile=icon.ico
UninstallDisplayIcon={app}\BatchGen.exe
UninstallDisplayName={#AppTitle}
Compression=lzma2/max
SolidCompression=yes
WizardStyle=modern
ShowLanguageDialog=no

[Languages]
Name: "zh"; MessagesFile: "compiler:Languages\ChineseSimplified.isl"

[Messages]
SelectDirBrowseLabel=生成的图片和视频默认也保存在这里，占用空间较大，建议选空间充足的磁盘。点击「下一步」继续。

[Tasks]
Name: "desktopicon"; Description: "创建桌面快捷方式"

[Files]
; 没有 ignoreversion：用旧安装包重装时，不会把自动更新后的新版本盖回去
Source: "..\dist\BatchGen.exe"; DestDir: "{app}"; Flags: replacesameversion
Source: "..\examples\示例.csv"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#WebView2Setup}"; DestName: "MicrosoftEdgeWebview2Setup.exe"; Flags: dontcopy

[Icons]
Name: "{autoprograms}\{#AppTitle}"; Filename: "{app}\BatchGen.exe"
Name: "{autodesktop}\{#AppTitle}"; Filename: "{app}\BatchGen.exe"; Tasks: desktopicon

[Run]
Filename: "{app}\BatchGen.exe"; Description: "打开 {#AppTitle}"; Flags: nowait postinstall skipifsilent

[UninstallDelete]
Type: files; Name: "{app}\BatchGen.old.exe"
Type: files; Name: "{app}\BatchGen.update"
Type: files; Name: "{app}\batchgen.log"
Type: filesandordirs; Name: "{app}\webview-data"

[Code]
const
  WebView2Client = 'Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}';

function HasRuntimeAt(Root: Integer; Key: String): Boolean;
var
  Version: String;
begin
  Result := RegQueryStringValue(Root, Key, 'pv', Version) and (Version <> '') and (Version <> '0.0.0.0');
end;

// 位置和判断方法来自微软的 WebView2 分发文档。
function HasWebView2: Boolean;
begin
#ifdef ForceWebView2
  // 只给测试用：这台电脑已有运行时，要走安装分支只能这样
  Result := False;
#else
  Result := HasRuntimeAt(HKLM32, 'SOFTWARE\' + WebView2Client) or HasRuntimeAt(HKCU, 'Software\' + WebView2Client);
#endif
end;

// 装不上不算安装失败：没有 WebView2 时程序会改用浏览器打开，还能用。
procedure InstallWebView2;
var
  Code: Integer;
begin
  WizardForm.StatusLabel.Caption := '正在安装 WebView2 运行组件，需要联网，请稍候…';
  WizardForm.ProgressGauge.Style := npbstMarquee;
  ExtractTemporaryFile('MicrosoftEdgeWebview2Setup.exe');
  if not Exec(ExpandConstant('{tmp}\MicrosoftEdgeWebview2Setup.exe'), '/silent /install', '', SW_HIDE, ewWaitUntilTerminated, Code)
    or (Code <> 0) then
    SuppressibleMsgBox('WebView2 运行组件没有装上（可能是网络问题）。' #13#10#13#10
      + 'BatchGen 仍然可以使用，会改用浏览器打开。以后也可以从微软官网安装 WebView2 Runtime。',
      mbInformation, MB_OK, IDOK);
  WizardForm.ProgressGauge.Style := npbstNormal;
end;

procedure CurStepChanged(CurStep: TSetupStep);
begin
  if (CurStep = ssPostInstall) and not HasWebView2 then
    InstallWebView2;
end;

function InsideFolder(Path, Folder: String): Boolean;
begin
  Path := Uppercase(AddBackslash(Path));
  Folder := Uppercase(AddBackslash(Folder));
  Result := Copy(Path, 1, Length(Folder)) = Folder;
end;

// Program Files 没有管理员权限写不进去，程序的数据和自动更新也都要写这个目录。
function NextButtonClick(CurPageID: Integer): Boolean;
begin
  Result := True;
  if (CurPageID = wpSelectDir)
    and (InsideFolder(WizardDirValue, ExpandConstant('{commonpf64}'))
      or InsideFolder(WizardDirValue, ExpandConstant('{commonpf32}'))) then
  begin
    SuppressibleMsgBox('不能安装到 Program Files，程序需要在安装目录里保存数据和更新自己。请换一个目录。',
      mbError, MB_OK, IDOK);
    Result := False;
  end;
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
begin
  if (CurUninstallStep = usPostUninstall) and DirExists(ExpandConstant('{app}')) and not UninstallSilent then
    MsgBox('生成的图片、视频和配置保留在：' #13#10 + ExpandConstant('{app}') + #13#10#13#10
      + '不需要的话可以手动删除这个文件夹。注意其中的 config.json 含有 API Key。', mbInformation, MB_OK);
end;
