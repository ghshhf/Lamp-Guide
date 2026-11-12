#ifndef AppVersion
  #define AppVersion "1.2.4"
#endif
#ifndef StageDir
  #error StageDir must point to the release staging directory
#endif
#ifndef RuntimeInstaller
  #error RuntimeInstaller must point to the signed Microsoft WebView2 offline installer
#endif

[Setup]
AppId={{18CA2B20-01BF-4E10-AC49-462C925D0321}
AppName=Lamp Guide
AppVersion={#AppVersion}
AppVerName=Lamp Guide {#AppVersion}
AppPublisher=Lamp Guide
DefaultDirName={localappdata}\Programs\Lamp Guide
DefaultGroupName=Lamp Guide
DisableWelcomePage=no
DisableDirPage=no
DisableProgramGroupPage=no
PrivilegesRequired=lowest
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
MinVersion=10.0
WizardStyle=modern
WizardSizePercent=115
WizardImageFile=assets\wizard.bmp
WizardSmallImageFile=assets\wizard-small.bmp
WizardImageStretch=yes
SetupIconFile=assets\lamp-guide.ico
UninstallDisplayIcon={app}\LampGuide.exe
UninstallDisplayName=Lamp Guide
InfoBeforeFile=install-info.txt
OutputDir=..\dist
OutputBaseFilename=LampGuide-Setup-{#AppVersion}-win-x64
Compression=lzma2/fast
SolidCompression=yes
AppMutex=Local\LampGuide.Running
SetupMutex=Local\LampGuide.Setup
CloseApplications=yes
RestartApplications=no
Uninstallable=yes
VersionInfoDescription=Lamp Guide 安装程序
VersionInfoProductName=Lamp Guide
VersionInfoVersion={#AppVersion}
AllowNoIcons=yes
LanguageDetectionMethod=uilanguage
ShowLanguageDialog=no

[Languages]
Name: "chinesesimplified"; MessagesFile: "ChineseSimplified.isl"
Name: "english"; MessagesFile: "compiler:Default.isl"

[CustomMessages]
chinesesimplified.CreateDesktopIcon=创建桌面快捷方式
chinesesimplified.LaunchApp=启动 Lamp Guide
chinesesimplified.ViewGuide=查看使用说明
chinesesimplified.RuntimeStatus=正在安装 Microsoft Edge WebView2 运行环境，请稍候……
chinesesimplified.RuntimeFailed=WebView2 运行环境未能安装成功（错误代码 %1）。请先安装微软官方 WebView2 运行环境，再重新运行此安装包。
english.CreateDesktopIcon=Create a desktop shortcut
english.LaunchApp=Launch Lamp Guide
english.ViewGuide=View the user guide
english.RuntimeStatus=Installing Microsoft Edge WebView2 Runtime. Please wait...
english.RuntimeFailed=WebView2 Runtime installation failed (error %1). Install the official Microsoft WebView2 Runtime, then run this installer again.

[Tasks]
Name: "desktopicon"; Description: "{cm:CreateDesktopIcon}"; GroupDescription: "{cm:AdditionalIcons}"

[Files]
Source: "{#StageDir}\LampGuide.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#StageDir}\使用说明.txt"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#StageDir}\THIRD-PARTY-NOTICES.txt"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\web\assets\lamp-guide.svg"; DestDir: "{app}\assets"; Flags: ignoreversion
Source: "assets\lamp-guide.ico"; DestDir: "{app}\assets"; Flags: ignoreversion
Source: "{#RuntimeInstaller}"; Flags: dontcopy noencryption

[Icons]
Name: "{group}\Lamp Guide"; Filename: "{app}\LampGuide.exe"; WorkingDir: "{app}"; AppUserModelID: "LampGuide.Desktop"
Name: "{group}\使用说明"; Filename: "{app}\使用说明.txt"
Name: "{group}\卸载 Lamp Guide"; Filename: "{uninstallexe}"
Name: "{autodesktop}\Lamp Guide"; Filename: "{app}\LampGuide.exe"; WorkingDir: "{app}"; AppUserModelID: "LampGuide.Desktop"; Tasks: desktopicon

[Registry]
Root: HKCU; Subkey: "Software\LampGuide"; ValueType: string; ValueName: "InstallPath"; ValueData: "{app}"; Flags: uninsdeletekey
Root: HKCU; Subkey: "Software\Microsoft\Windows\CurrentVersion\App Paths\LampGuide.exe"; ValueType: string; ValueName: ""; ValueData: "{app}\LampGuide.exe"; Flags: uninsdeletekey

[Run]
Filename: "{app}\使用说明.txt"; Description: "{cm:ViewGuide}"; Flags: postinstall shellexec skipifsilent unchecked
Filename: "{app}\LampGuide.exe"; Description: "{cm:LaunchApp}"; Flags: nowait postinstall skipifsilent

[Code]
const
  RunKey = 'Software\Microsoft\Windows\CurrentVersion\Run';
  RuntimeKey = 'Software\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}';

function HasRuntimeAt(Root: Integer): Boolean;
var
  Version: String;
begin
  Result := RegQueryStringValue(Root, RuntimeKey, 'pv', Version) and
    (Version <> '') and (Version <> '0.0.0.0');
end;

function HasWebView2: Boolean;
begin
  Result := HasRuntimeAt(HKLM32) or HasRuntimeAt(HKLM64) or
    HasRuntimeAt(HKCU32) or HasRuntimeAt(HKCU64);
end;

function PrepareToInstall(var NeedsRestart: Boolean): String;
var
  ExitCode: Integer;
begin
  Result := '';
  if HasWebView2 then Exit;
  WizardForm.StatusLabel.Caption := CustomMessage('RuntimeStatus');
  ExtractTemporaryFile('MicrosoftEdgeWebView2RuntimeInstallerX64.exe');
  if not Exec(ExpandConstant('{tmp}\MicrosoftEdgeWebView2RuntimeInstallerX64.exe'),
    '/silent /install', '', SW_HIDE, ewWaitUntilTerminated, ExitCode) then
  begin
    Result := FmtMessage(CustomMessage('RuntimeFailed'), [IntToStr(ExitCode)]);
    Exit;
  end;
  if ExitCode = 3010 then NeedsRestart := True;
  if not HasWebView2 then
    Result := FmtMessage(CustomMessage('RuntimeFailed'), [IntToStr(ExitCode)]);
end;

procedure CurStepChanged(CurStep: TSetupStep);
var
  Previous: String;
begin
  if CurStep <> ssPostInstall then Exit;
  // Keep an existing startup preference, but point it to the installed file.
  if RegQueryStringValue(HKCU, RunKey, 'Lamp Guide', Previous) or
     RegQueryStringValue(HKCU, RunKey, 'Zen Gate', Previous) then
  begin
    if Previous <> '' then
    begin
      if RegWriteStringValue(HKCU, RunKey, 'Lamp Guide',
        '"' + ExpandConstant('{app}\LampGuide.exe') + '"') then
        RegDeleteValue(HKCU, RunKey, 'Zen Gate');
    end;
  end;
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
var
  Existing: String;
begin
  if CurUninstallStep <> usUninstall then Exit;
  if RegQueryStringValue(HKCU, RunKey, 'Lamp Guide', Existing) and
    (CompareText(Existing, '"' + ExpandConstant('{app}\LampGuide.exe') + '"') = 0) then
    RegDeleteValue(HKCU, RunKey, 'Lamp Guide');
end;
