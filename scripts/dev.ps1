<#
.SYNOPSIS
  一键启动 / 停掉 DeepTalk 全套开发环境。

.DESCRIPTION
  ⚠️ 这个文件必须保存为 **UTF-8 with BOM**。
     Windows PowerShell 5.1 在没有 BOM 时按系统 OEM 代码页（中文机器是 GBK）
     解码 .ps1，中文注释和提示会整片变成乱码，进而把引号括号都搞错位，
     报出一堆"意外的标记 }"之类的语法错 —— 那些报错位置全是假的，
     真正的原因只是文件开头的 3 个字节。
     PowerShell 7 默认按 UTF-8 读，没有这个问题，所以**用 7 测不出这个坑**。
     改完这个文件记得补回 BOM，或者统一用 scripts/dev.cmd 启动（它优先用 pwsh）。

  管四样东西，但它们**跑在两个不同的地方**，这是本脚本存在的理由：

      Windows                        WSL2 Ubuntu
      ├── 后端   :9090               └── Docker
      ├── MCP    :8081                   ├── mysql    :3306 → 3307
      ├── 前端   :8080                   ├── redis    :6379 → 6380
      └── 本地连接器                      └── rabbitmq :5672 → 5673

  所以脚本做三件事：探测中间件（地址从 config/config.toml 读，不猜）、
  在 Windows 侧起三个服务进程、把状态汇总成一张表。

.PARAMETER Check
  只体检，不启动任何东西。换机器 / 出问题时先跑这个。

.PARAMETER Stop
  停掉 Windows 侧的服务进程（按端口找）。加 -Middleware 连中间件一起停。

.PARAMETER Middleware
  在 WSL 里执行 docker compose up -d mysql redis rabbitmq。
  不加这个参数时，脚本只**检查**中间件在不在，不尝试启动它 ——
  因为你的中间件不一定是 Docker 起的（可能是 apt 装的、或者 Windows 原生装的），
  擅自拉起第二份会把端口和数据的归属搞乱。

.PARAMETER Rebind
  检测到 config.toml 里的中间件地址指向一个已失效的 WSL IP 时，
  自动改写成本次启动的 WSL IP。

.PARAMETER NoFrontend
  不起前端（只调后端接口时省一个窗口）。

.PARAMETER LocalAgent
  顺带起本地工作区连接器。需要先做过一次性的 token 配置，
  见 `go run ./cmd/localagent -h`。

.EXAMPLE
  .\scripts\dev.ps1 -Check
  .\scripts\dev.ps1
  .\scripts\dev.ps1 -Middleware -Rebind
  .\scripts\dev.ps1 -Stop
#>
[CmdletBinding()]
param(
    [switch]$Check,
    [switch]$Stop,
    [switch]$Middleware,
    [switch]$Rebind,
    [switch]$NoFrontend,
    [switch]$LocalAgent
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot   # scripts/ 的上一层 = 项目根
Set-Location $root

# 复用当前正在跑的 PowerShell（用户用 pwsh 跑就用 pwsh，powershell 就用 powershell）
$ShellExe = (Get-Process -Id $PID).Path

# 端口 → 用途。后端/MCP/前端这三个是 Windows 侧的，由本脚本启动。
$Ports = @{
    Backend  = 9090
    Mcp      = 8081
    Frontend = 8080
}

# ==================== 小工具 ====================

function Write-Head([string]$Text) {
    Write-Host ""
    Write-Host "── $Text " -ForegroundColor Cyan -NoNewline
    Write-Host ("─" * [Math]::Max(0, 60 - $Text.Length)) -ForegroundColor DarkGray
}

# Test-Port 探测 TCP 是否可连。
#
# 不用 Test-NetConnection：它每个端口要 1~3 秒，探测 6 个端口就是十几秒，
# 而这里只想知道"通不通"，800ms 足够。
function Test-Port([string]$Target, [int]$Port, [int]$TimeoutMs = 800) {
    $client = New-Object System.Net.Sockets.TcpClient
    try {
        $task = $client.ConnectAsync($Target, $Port)
        return $task.Wait($TimeoutMs) -and $client.Connected
    } catch {
        return $false
    } finally {
        $client.Dispose()
    }
}

# Get-Listener 找出占用某端口的进程（用于 -Stop 与"已有人在跑"的判断）
function Get-Listener([int]$Port) {
    $conn = Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue |
        Select-Object -First 1
    if (-not $conn) { return $null }
    $proc = Get-Process -Id $conn.OwningProcess -ErrorAction SilentlyContinue
    return [pscustomobject]@{
        PID  = $conn.OwningProcess
        Name = if ($proc) { $proc.ProcessName } else { 'unknown' }
    }
}

# Get-TomlValue 从 config.toml 里读某个 section 下的某个键。
#
# 为什么不硬编码端口：你的中间件地址是**分散**的 ——
# Redis 指向 WSL 的 IP，MySQL 和 RabbitMQ 指向 127.0.0.1。
# 猜错地址比不检查更糟：脚本会说"中间件没问题"，而实际连不上。
function Get-TomlValue([string[]]$Lines, [string]$Section, [string]$Key) {
    $inSection = $false
    foreach ($line in $Lines) {
        $t = ($line -replace '#.*$', '').Trim()   # 去掉行尾注释
        if ($t -eq '') { continue }
        if ($t -match '^\[+([^\]]+)\]+') {
            $inSection = ($Matches[1].Trim() -eq $Section)
            continue
        }
        if (-not $inSection) { continue }
        if ($t -match "^$Key\s*=\s*`"?([^`"]*)`"?\s*$") {
            return $Matches[1].Trim()
        }
    }
    return $null
}

# Start-Svc 在一个新窗口里起一个服务。
#
# 为什么要新窗口：这三个都是长驻的、会持续打日志的进程。
# 塞进同一个窗口就没法看谁在说什么；重定向到文件又失去了"出错了立刻看见"。
#
# -NoExit：崩溃后的错误信息留在屏幕上，而不是窗口一闪而过。
#
# -NoProfile：**这个参数是踩坑加上的**。用户报过子窗口里
#   "术语 'go' 不会被识别为 cmdlet…"，而父进程预检明明是过的。
#   原因指向用户的 PowerShell profile 改写了 $env:PATH —— 父进程因为
#   dev.cmd 传了 -NoProfile 而免疫，子窗口却把 profile 加载了。
#   顺便也让子窗口的行为可预测：不会被个人配置影响。
function Start-Svc([string]$Title, [string]$WorkDir, [string]$Command) {
    Write-Host "  → $Title" -ForegroundColor Green

    # 显式把工具目录注入子进程 PATH（见上面 ToolPathPrefix 的说明）。
    # 单引号包住，路径里不会有单引号，所以不用做转义。
    $pathFix = if ($script:ToolPathPrefix) {
        "`$env:PATH = '$($script:ToolPathPrefix);' + `$env:PATH; "
    } else { '' }

    $inner = "$pathFix`$host.UI.RawUI.WindowTitle = '$Title'; Set-Location '$WorkDir'; $Command"
    Start-Process -FilePath $ShellExe -ArgumentList @(
        '-NoLogo', '-NoProfile', '-NoExit', '-ExecutionPolicy', 'Bypass', '-Command', $inner
    ) | Out-Null
}

# ==================== 1. 前置检查 ====================

Write-Head "环境"

if (-not (Test-Path 'config/config.toml')) {
    Write-Host "  ✗ 找不到 config/config.toml" -ForegroundColor Red
    Write-Host "    从模板复制一份：copy config\config.toml.example config\config.toml"
    exit 1
}
$cfgLines = Get-Content 'config/config.toml'

# 解析出 go / node / npm 的**绝对路径**，而不是只确认"它们能用"。
#
# 为什么：子窗口里报过 "术语 'go' 不会被识别" —— 父进程找得到 go，
# 子窗口却找不到。最可能的原因是 PowerShell profile 改写了 $env:PATH
# （父进程因为 dev.cmd 传了 -NoProfile 而免疫，子窗口没有）。
#
# 但不管真因是哪一个，**记住绝对路径**都能绕开：
# 子进程的 PATH 再怎么变，绝对路径照样能执行。
$GoExe   = (Get-Command go -ErrorAction SilentlyContinue).Source
$NodeExe = (Get-Command node -ErrorAction SilentlyContinue).Source
$NpmExe  = (Get-Command npm.cmd -ErrorAction SilentlyContinue).Source
if (-not $NpmExe) { $NpmExe = (Get-Command npm -ErrorAction SilentlyContinue).Source }

foreach ($pair in @(@('go', $GoExe), @('node', $NodeExe))) {
    if (-not $pair[1]) {
        Write-Host "  ✗ 找不到 $($pair[0])，它在 PATH 里吗？" -ForegroundColor Red
        exit 1
    }
}
Write-Host "  ✓ go    $GoExe"
Write-Host "  ✓ node  $NodeExe"

# 把工具所在目录显式传给子进程的 PATH。
#
# 光靠父进程 PATH 里有还不够：npm 会自己 fork node，`go` 也会去调工具链里
# 别的可执行文件 —— 它们都靠 PATH 找。所以在子命令开头就把这几个目录拼进去，
# 比依赖继承可靠。
$script:ToolPathPrefix = (
    @($GoExe, $NodeExe, $NpmExe) |
        Where-Object { $_ } |
        ForEach-Object { Split-Path -Parent $_ } |
        Select-Object -Unique
) -join ';'

if (-not $NoFrontend -and -not $NpmExe) {
    Write-Host "  ! 找不到 npm，跳过前端" -ForegroundColor Yellow
    $NoFrontend = $true
}

if (-not $NoFrontend -and -not (Test-Path 'vue-frontend/node_modules')) {
    Write-Host "  ! 前端依赖没装，先执行：" -ForegroundColor Yellow
    Write-Host "      cd vue-frontend; npm install"
    $NoFrontend = $true
}

# ==================== 2. WSL 地址漂移检查 ====================

function Get-WslIP {
    try {
        # hostname -I 输出当前 WSL 虚拟机的所有 IPv4，取第一个
        $out = & wsl.exe -e bash -lc "hostname -I" 2>$null
        if ($LASTEXITCODE -ne 0 -or -not $out) { return $null }
        return ($out -split '\s+' | Where-Object { $_ -match '^\d+\.\d+\.\d+\.\d+$' } | Select-Object -First 1)
    } catch {
        return $null
    }
}

$wslIP = Get-WslIP

# 中间件清单：从 config.toml 读地址，避免猜错。
#
# ⚠️ 变量名别叫 $middleware。PowerShell 的变量名**大小写不敏感**，
#    它和上面 param 里的 [switch]$Middleware 是同一个变量 ——
#    赋值数组会被类型约束拒绝，而且报的错是
#    "无法将 System.Object[] 转换为 SwitchParameter"，
#    完全不会提到变量名，极难定位。（这个坑我自己踩过一次。）
$deps = @(
    [pscustomobject]@{ Name = 'MySQL';    Section = 'mysqlConfig';       DefaultPort = 3306 }
    [pscustomobject]@{ Name = 'Redis';    Section = 'redisConfig';       DefaultPort = 6379 }
    [pscustomobject]@{ Name = 'RabbitMQ'; Section = 'rabbitmqConfig';    DefaultPort = 5672 }
    # MCP 服务端的地址写在 url 里，且它在 [[mcpConfig.servers]] 这张子表下，
    # 所以 section 名是点号形式，不是 mcpConfig
    [pscustomobject]@{ Name = 'MCP 天气'; Section = 'mcpConfig.servers'; DefaultPort = 8081 }
)

Write-Head "中间件（地址取自 config/config.toml）"

if ($wslIP) {
    Write-Host "  当前 WSL IP: $wslIP" -ForegroundColor DarkGray
} else {
    Write-Host "  (拿不到 WSL IP —— 从 Windows 调 wsl.exe 失败，跳过漂移检查)" -ForegroundColor DarkGray
}

$drifted = @()
foreach ($m in $deps) {
    $mHost = Get-TomlValue $cfgLines $m.Section 'host'
    $url   = Get-TomlValue $cfgLines $m.Section 'url'
    $port  = Get-TomlValue $cfgLines $m.Section 'port'

    # MCP 服务端的地址写在 url 里，不是 host/port
    if ($url) {
        if ($url -match '://([^:/]+)(?::(\d+))?') {
            $mHost = $Matches[1]
            if ($Matches[2]) { $port = $Matches[2] }
        }
    }
    if (-not $port) { $port = $m.DefaultPort }
    if (-not $mHost) { $mHost = '127.0.0.1' }
    $port = [int]$port

    $ok = Test-Port $mHost $port
    $mark = if ($ok) { '✓' } else { '✗' }
    $color = if ($ok) { 'Green' } else { 'Red' }
    Write-Host ("  {0} {1,-10} {2}:{3}" -f $mark, $m.Name, $mHost, $port) -ForegroundColor $color

    # 漂移判定：地址是 172.x（WSL 网段）但不是当前 WSL IP
    if ($mHost -match '^172\.' -and $wslIP -and $mHost -ne $wslIP) {
        $drifted += [pscustomobject]@{ Section = $m.Section; Name = $m.Name; Old = $mHost; New = $wslIP }
    }
}

if ($drifted.Count -gt 0) {
    Write-Host ""
    Write-Host "  ⚠ 有地址指向了**过期的 WSL IP**（WSL 重启后会变）：" -ForegroundColor Yellow
    foreach ($d in $drifted) {
        Write-Host ("      {0,-10} config.toml [{1}] host = `"{2}`"  →  应为 `"{3}`"" -f `
            $d.Name, $d.Section, $d.Old, $d.New) -ForegroundColor Yellow
    }

    if ($Rebind) {
        $new = Get-Content 'config/config.toml'
        foreach ($d in $drifted) {
            $inSection = $false
            for ($i = 0; $i -lt $new.Count; $i++) {
                $t = $new[$i].Trim()
                if ($t -match '^\[([^\]]+)\]') { $inSection = ($Matches[1] -eq $d.Section); continue }
                if ($inSection -and $t -match '^host\s*=') {
                    $new[$i] = $new[$i] -replace [regex]::Escape($d.Old), $d.New
                    break
                }
            }
        }
        Set-Content 'config/config.toml' -Value $new -Encoding UTF8
        Write-Host "  ✓ 已改写 config/config.toml（原文件未备份，如需回退用 git diff 看）" -ForegroundColor Green
    } else {
        Write-Host "      加 -Rebind 可以让脚本自动改写。" -ForegroundColor Yellow
    }
}

if ($Middleware -and -not $Check) {
    Write-Host ""
    Write-Host "  在 WSL 里启动中间件..." -ForegroundColor Green
    # Windows 路径转 WSL 路径：C:\a\b → /mnt/c/a/b
    $wslPath = if ($root -match '^([A-Za-z]):\\(.*)$') {
        "/mnt/$($Matches[1].ToLower())/$($Matches[2] -replace '\\','/')"
    } else {
        $root -replace '\\', '/'
    }
    # 只起中间件三个服务：backend / frontend 走 Windows 本机进程，
    # 这样改代码立刻生效，不用重建镜像。
    & wsl.exe -e bash -lc "cd '$wslPath' && docker compose up -d mysql redis rabbitmq"
    if ($LASTEXITCODE -ne 0) {
        Write-Host "  ✗ WSL 里 docker compose 失败。常见原因：" -ForegroundColor Red
        Write-Host "      - WSL 里没装 docker，或当前用户不在 docker 组（要 sudo）"
        Write-Host "      - 项目路径不对（WSL 里能看到 $wslPath 吗）"
    }
}

# ==================== 3. 体检模式到此为止 ====================

if ($Check) {
    Write-Head "Windows 侧服务"
    foreach ($name in $Ports.Keys) {
        $p = $Ports[$name]
        $l = Get-Listener $p
        if ($l) {
            Write-Host ("  ✓ {0,-10} :{1}  {2} (PID {3})" -f $name, $p, $l.Name, $l.PID) -ForegroundColor Green
        } else {
            Write-Host ("  - {0,-10} :{1}  未运行" -f $name, $p) -ForegroundColor DarkGray
        }
    }
    Write-Host ""
    Write-Host "  想启动：.\scripts\dev.ps1" -ForegroundColor Cyan
    exit 0
}

# ==================== 4. 停掉 ====================

if ($Stop) {
    Write-Head "停止"
    foreach ($name in @('Backend', 'Mcp', 'Frontend')) {
        $p = $Ports[$name]
        $l = Get-Listener $p
        if ($l) {
            # 打印进程名再杀：端口可能被别的东西占着，
            # 用户看到 "杀掉 node" 和 "杀掉 mysqld" 的反应应该不一样。
            Write-Host ("  停止 {0,-10} :{1}  {2} (PID {3})" -f $name, $p, $l.Name, $l.PID) -ForegroundColor Yellow
            Stop-Process -Id $l.PID -Force -ErrorAction SilentlyContinue
        } else {
            Write-Host ("  {0,-10} :{1}  本来就没跑" -f $name, $p) -ForegroundColor DarkGray
        }
    }
    if ($Middleware) {
        Write-Host "  在 WSL 里停中间件..." -ForegroundColor Yellow
        $wslPath = if ($root -match '^([A-Za-z]):\\(.*)$') {
            "/mnt/$($Matches[1].ToLower())/$($Matches[2] -replace '\\','/')"
        } else { $root -replace '\\', '/' }
        & wsl.exe -e bash -lc "cd '$wslPath' && docker compose stop mysql redis rabbitmq"
    } else {
        Write-Host "  (中间件没停。要一起停加 -Middleware)" -ForegroundColor DarkGray
    }
    exit 0
}

# ==================== 5. 启动 ====================

Write-Head "启动"

# 端口已在响应时跳过，而不是报错退出：
# 重复跑这个脚本是很自然的（"我改完代码再起一遍"），
# 因为"已经开着"而失败会让人以为脚本坏了。
#
# 判据用 Test-Port 而不是 Get-Listener：WSL2 会把 WSL 里的端口转发到
# Windows 的 localhost，那种端口**能连上但 Windows 上没有监听进程**，
# 用 Get-Listener 判会误判成"没跑"然后再起一个，直接撞端口。
if (Test-Port '127.0.0.1' $Ports.Mcp) {
    Write-Host "  - MCP :8081 已在响应，跳过" -ForegroundColor DarkGray
} else {
    Start-Svc 'DeepTalk MCP' $root "& '$GoExe' run ./internal/infra/mcp -http-addr :8081"
}

if (Test-Port '127.0.0.1' $Ports.Backend) {
    Write-Host "  - 后端 :9090 已在响应，跳过" -ForegroundColor DarkGray
} else {
    Start-Svc 'DeepTalk 后端' $root "& '$GoExe' run ./cmd/server"
}

if (-not $NoFrontend) {
    if (Test-Port '127.0.0.1' $Ports.Frontend) {
        Write-Host "  - 前端 :8080 已在响应，跳过" -ForegroundColor DarkGray
    } else {
        Start-Svc 'DeepTalk 前端' (Join-Path $root 'vue-frontend') "& '$NpmExe' run serve"
    }
}

if ($LocalAgent) {
    Start-Svc 'DeepTalk 本地连接器' $root "& '$GoExe' run ./cmd/localagent"
}

# ==================== 6. 等就绪 ====================

Write-Head "等待就绪"

# 后端要先编译（首次 10~30 秒），所以等久一点；MCP 同理。
$waits = @(
    [pscustomobject]@{ Name = 'MCP';    Port = 8081; Secs = 40 }
    [pscustomobject]@{ Name = '后端';   Port = 9090; Secs = 90 }
    [pscustomobject]@{ Name = '前端';   Port = 8080; Secs = 60 }
)
foreach ($w in $waits) {
    if ($w.Name -eq '前端' -and $NoFrontend) { continue }

    $sw = [Diagnostics.Stopwatch]::StartNew()
    $ok = $false
    while ($sw.Elapsed.TotalSeconds -lt $w.Secs) {
        if (Test-Port '127.0.0.1' $w.Port 500) { $ok = $true; break }
        Start-Sleep -Milliseconds 500
    }
    if ($ok) {
        Write-Host ("  ✓ {0,-6} :{1}  就绪（{2:N1}s）" -f $w.Name, $w.Port, $sw.Elapsed.TotalSeconds) -ForegroundColor Green
    } else {
        Write-Host ("  ✗ {0,-6} :{1}  超时 {2}s —— 去看它的窗口里的报错" -f $w.Name, $w.Port, $w.Secs) -ForegroundColor Red
    }
}

# ==================== 7. 汇总 ====================

Write-Head "地址"
Write-Host "  前端        http://localhost:8080"
Write-Host "  后端        http://localhost:9090"
Write-Host "  指标        http://localhost:9090/metrics"
Write-Host "  MCP         http://localhost:8081/mcp"
Write-Host "  Grafana     http://localhost:3000   (admin / admin)"
Write-Host "  Prometheus  http://localhost:9091"
Write-Host "  RabbitMQ    http://localhost:15673  (root / 123456)"
Write-Host ""
Write-Host "  停掉        .\scripts\dev.ps1 -Stop" -ForegroundColor Cyan
