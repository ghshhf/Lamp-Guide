# Lamp Guide Windows 安装包

当前产品版本：1.2.3。目标：Windows 10 / 11 x64，当前用户安装。

## 安装流程

欢迎 → 使用与数据说明 → 安装位置 → 开始菜单 → 桌面快捷方式 → 确认 → 安装 → 完成。
缺少 WebView2 时，安装器先调用内附的微软官方 Evergreen x64 离线安装程序。
完成页可选择启动应用、查看使用说明。可从 Windows 应用列表或开始菜单卸载。
卸载保留账号、配置、WebView2 缓存和共享 WebView2 运行环境。

## 构建依赖

- Go 1.26、Node.js 22+。
- Inno Setup 6.7.4：从 https://jrsoftware.org/isdl.php 获取；验证 Authenticode 签名后安装。
- `go install github.com/tc-hib/go-winres@v0.3.3`。
- 微软 Evergreen WebView2 x64 离线安装器：从 https://developer.microsoft.com/microsoft-edge/webview2/#download 获取；保存为 `MicrosoftEdgeWebView2RuntimeInstallerX64.exe`。
- 在 `packaging/` 执行 `npm ci --ignore-scripts`。

构建脚本默认从 `_refs/build-tools/` 找工具和 WebView2，也可传参数：

```powershell
.\tools\build-release.ps1 -Version 1.2.3 -Iscc 'C:\path\ISCC.exe' -Winres 'C:\path\go-winres.exe' -RuntimeInstaller 'C:\path\MicrosoftEdgeWebView2RuntimeInstallerX64.exe'
```

输出：`dist/LampGuide-Setup-1.2.3-win-x64.exe` 及 SHA-256 校验文件。
脚本仅打包 `dist/app/` 中明确列出的程序、说明、第三方声明和品牌资源，不读取用户配置。
运行环境签名必须有效且签发给 Microsoft Corporation，否则停止构建。

`web/assets/lamp-guide.svg` 是手写图标源文件。`tools/build-assets.cjs` 从它生成多尺寸 ICO、预览 PNG 和安装向导 BMP；`winres.json` 将图标、版本和 DPI 清单嵌入 EXE。
`ChineseSimplified.isl` 来自 Inno Setup 6.7.4 官方源码仓库的 `Files/Languages/Unofficial/`。

## 兼容边界

品牌、程序文件名和 Windows 应用身份使用 Lamp Guide。
网关数据目录 `zen-gate`、WebView2 目录 `ZenUI`、虚拟来源 `https://zen.ui`、环境变量、API 协议标识和 Go 模块名继续保留，保证已有登录、客户端接入和主题记忆兼容。
开机启动选项支持旧 `Zen Gate` 名称，启用或升级后指向已安装的 `LampGuide.exe`。
安装和卸载检查 `Local\LampGuide.Running`，要求正常关闭正在运行的新版应用。

安装包目前未配置代码签名证书；最终 EXE 的签名状态为未签名。

构建和分发前请确认所用 Inno Setup 版本的授权范围，本地工具的授权不会随项目源码转让。应用自身与参考组件的许可见根目录 `LICENSE`、`THIRD_PARTY.md` 和 `licenses/`；完整声明由构建脚本汇总并随安装包提供。
