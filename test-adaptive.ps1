#Requires -Version 5.1
<#
.SYNOPSIS
  验证 blind-llm-eyes P2 自适应限流（adaptive_concurrency）的端到端效果。

.DESCRIPTION
  - 启动前请先用下方 [RECOMMENDED CONFIG] 覆盖 config.yaml，再重启 blind-llm-eyes.exe
  - 本脚本每轮生成 N 张 **不同颜色** 的 1x1 像素真 PNG（字节不同 → cache hash 不同 → 每张都触发真实 MiMo 调用 = 1 个样本）
  - 每轮发送 1 个 Anthropic messages 请求，POST 到 $BaseUrl/v1/messages
  - 轮询 /metrics 端点解析：
      * blind_llm_eyes_adaptive_concurrency_current （当前 effective_limit）
      * blind_llm_eyes_adaptive_vision_p90_seconds （最近一次评估窗口 P90）
      * blind_llm_eyes_adaptive_concurrency_adjustments_total （累计调节方向计数）
  - 打印实时表格，并在最后输出 CSV 便于复制分析。

.EXAMPLE
  # 快速验证（默认参数）：4 轮 × 每轮 4 张 → 预期观察 limit 4 → 3 → 2 → 1（逐步下降）
  .\test-adaptive.ps1

  # 更长压力：每轮 6 张，10 轮
  .\test-adaptive.ps1 -ImagesPerRequest 6 -Iterations 10

  # 换端口
  .\test-adaptive.ps1 -BaseUrl "http://127.0.0.1:9000"
#>

[CmdletBinding()]
param(
    [string]$BaseUrl = "http://127.0.0.1:8790",
    [int]$ImagesPerRequest = 4,
    [int]$Iterations = 4,
    [int]$SleepBetweenIterationsMs = 1500,
    [int]$RequestTimeoutSec = 180
)

$ErrorActionPreference = "Stop"
Add-Type -AssemblyName System.Drawing

# ============================================================
#  [RECOMMENDED CONFIG] —— 把下面这节贴到 config.yaml 末尾，
#                         然后重启 blind-llm-eyes.exe。
#                         这套参数保证 DECREASE 可在 4 轮内复现。
# ============================================================
$recommendedConfig = @'
concurrency_limit: 4          # 静态初始值（也是 adaptive 的 max_limit 上限）

adaptive_concurrency:
  enabled: true
  min_limit: 1
  max_limit: 4
  fast_threshold_ms: 1000     # 故意设得极低 → 现实 MiMo (≥8s) 永远进不到 "fast" 分支
  slow_threshold_ms: 3000     # 故意设低 → 现实 MiMo (≥8s) 每一批都判定 tooSlow → 触发 ×0.75 下降
  sample_window: 4            # 窗口很小 → 1 个请求（每轮 4 张）就填满一次窗口，立刻评估
  cooldown_ms: 1000           # 1s 冷却，脚本每轮之间 sleep 1.5s 保证有资格重新评估
  increase_step: 1
  decrease_ratio: 0.75
  error_threshold: 0.1
'@

# ============================================================
#  帮助函数
# ============================================================

function Write-Section {
    param([string]$Text)
    Write-Host ""
    Write-Host ("=" * 72) -ForegroundColor DarkCyan
    Write-Host "  $Text" -ForegroundColor Cyan
    Write-Host ("=" * 72) -ForegroundColor DarkCyan
}

function Write-Highlight {
    param([string]$Text, [ConsoleColor]$Color = [ConsoleColor]::Yellow)
    Write-Host "  => $Text" -ForegroundColor $Color
}

function New-TinyPngBase64 {
    <# 创建一张 1x1 像素的真 PNG（不同 RGB = 不同字节 = 不同 cache hash） #>
    param([byte]$R, [byte]$G, [byte]$B)
    try {
        $bmp = New-Object System.Drawing.Bitmap(1, 1)
        $bmp.SetPixel(0, 0, [System.Drawing.Color]::FromArgb(255, $R, $G, $B))
        $ms = New-Object System.IO.MemoryStream
        $bmp.Save($ms, [System.Drawing.Imaging.ImageFormat]::Png)
        $bytes = $ms.ToArray()
        return [Convert]::ToBase64String($bytes)
    } finally {
        if ($bmp) { $bmp.Dispose() }
        if ($ms)  { $ms.Dispose()  }
    }
}

function New-ImageRequestBody {
    <# 构造 1 个 /v1/messages 请求体（对应 test 里的 buildNImageRequest） #>
    param(
        [int]$ImageCount,
        [int]$ColorSeed  # 确保不同请求的图片颜色完全不同
    )
    $content = New-Object System.Collections.Generic.List[object]
    $content.Add(@{ type = "text"; text = "Describe these images briefly." })

    for ($i = 0; $i -lt $ImageCount; $i++) {
        $total = $ColorSeed * $ImageCount + $i
        $r = [byte](($total * 37) -band 0xFF)
        $g = [byte](($total * 73) -band 0xFF)
        $b = [byte](($total * 131) -band 0xFF)
        $b64 = New-TinyPngBase64 -R $r -G $g -B $b
        $content.Add(@{
            type   = "image"
            source = @{
                type       = "base64"
                media_type = "image/png"
                data       = $b64
            }
        })
    }

    $body = @{
        model      = "claude-3-5-sonnet-20241022"
        max_tokens = 10          # 仅触发最少的 upstream 成本，不关心内容
        stream     = $false
        messages   = @(
            @{ role = "user"; content = $content.ToArray() }
        )
    }
    return ($body | ConvertTo-Json -Depth 10 -Compress)
}

function Get-AdaptiveMetrics {
    <# 解析 /metrics 端点，返回 adaptive 相关 3 个指标 #>
    $raw = try { Invoke-RestMethod -Uri "$BaseUrl/metrics" -Method Get -TimeoutSec 10 } catch { return $null }
    $lines = $raw -split "`n"

    $cur = $null
    $p90 = $null
    $up = 0; $down = 0; $none = 0

    foreach ($line in $lines) {
        if ($line -match '^blind_llm_eyes_adaptive_concurrency_current\s+([0-9.]+)') {
            $cur = [int][double]$Matches[1]
        }
        if ($line -match '^blind_llm_eyes_adaptive_vision_p90_seconds\s+([0-9.]+)') {
            $p90 = [double]$Matches[1]
        }
        if ($line -match '^blind_llm_eyes_adaptive_concurrency_adjustments_total\{direction="up"\}\s+([0-9.]+)') {
            $up = [int][double]$Matches[1]
        }
        if ($line -match '^blind_llm_eyes_adaptive_concurrency_adjustments_total\{direction="down"\}\s+([0-9.]+)') {
            $down = [int][double]$Matches[1]
        }
        if ($line -match '^blind_llm_eyes_adaptive_concurrency_adjustments_total\{direction="none"\}\s+([0-9.]+)') {
            $none = [int][double]$Matches[1]
        }
    }
    return [pscustomobject]@{
        CurrentLimit = $cur
        P90Seconds   = $p90
        AdjustUp     = $up
        AdjustDown   = $down
        AdjustNone   = $none
    }
}

function Invoke-ImageRequest {
    param([string]$Body, [int]$IterationId)
    try {
        $resp = Invoke-RestMethod -Uri "$BaseUrl/v1/messages" -Method Post `
            -ContentType "application/json" -Body $Body -TimeoutSec $RequestTimeoutSec
        $content = if ($resp.content -is [string]) { $resp.content }
                   elseif ($resp.content -is [array] -and $resp.content.Count -gt 0) {
                       ($resp.content | ForEach-Object { $_.text }) -join ""
                   }
                   else { [string]$resp }
        return [pscustomobject]@{
            Ok = $true
            Status = 200
            Preview = ($content -replace '\s+', ' ').Trim().Substring(0, [Math]::Min(140, ($content.Length)))
        }
    } catch {
        $status = if ($_.Exception.Response) { [int]$_.Exception.Response.StatusCode } else { 0 }
        try {
            $reader = New-Object System.IO.StreamReader($_.Exception.Response.GetResponseStream())
            $errBody = $reader.ReadToEnd(); $reader.Dispose()
            $preview = ($errBody -replace '\s+', ' ').Trim()
            if ($preview.Length -gt 200) { $preview = $preview.Substring(0, 200) + "..." }
        } catch { $preview = $_.Exception.Message }
        return [pscustomobject]@{
            Ok = ($false -band ($status -ne 0))
            Status = $status
            Preview = $preview
        }
    }
}

# ============================================================
#  主流程
# ============================================================

Write-Section "P2 自适应限流（adaptive_concurrency）端到端验证脚本"
Write-Host "  Target       : $BaseUrl"
Write-Host "  Iterations   : $Iterations 轮（每轮 1 个请求 × $ImagesPerRequest 张独特图片）"
Write-Host "  Sleep/轮     : $SleepBetweenIterationsMs ms"
Write-Host "  总 Vision 调用: $($Iterations * $ImagesPerRequest) 次真实 MiMo"
Write-Host ""
Write-Host "  为了节省 DeepSeek 成本，可把 config.yaml 中 upstream.base_url"
Write-Host "  临时换成 http://127.0.0.1:1/ 这种坏地址。"
Write-Host "  （Vision 处理发生在 upstream 之前，完全不影响 adaptive 采样）"
Write-Host ""
Write-Host "  [RECOMMENDED CONFIG] 建议复制到 config.yaml 末尾并重启服务：" -ForegroundColor DarkYellow
Write-Host $recommendedConfig -ForegroundColor DarkGray
Write-Host ""

Write-Section "Step 0 / Pre-flight"

try {
    $health = Invoke-RestMethod -Uri "$BaseUrl/healthz" -TimeoutSec 5
    Write-Highlight "/healthz => OK" Green
} catch {
    Write-Highlight "连接失败：请先启动 blind-llm-eyes.exe（./blind-llm-eyes.exe），确认监听 $BaseUrl" Red
    exit 1
}

$m0 = Get-AdaptiveMetrics
if (-not $m0 -or $null -eq $m0.CurrentLimit) {
    Write-Highlight "/metrics 里找不到 adaptive 指标 → adaptive_concurrency.enabled 可能是 false，请按 [RECOMMENDED CONFIG] 修改 config.yaml 后重启进程。" Red
    exit 2
}
Write-Highlight "初始 metrics: effective_limit=$($m0.CurrentLimit), P90=$($m0.P90Seconds)s, adjustments(up/down/none)=($($m0.AdjustUp)/$($m0.AdjustDown)/$($m0.AdjustNone))" Green

# 检测进程是否未重启（上次运行的累积 adjustments 仍在）
if ($m0.AdjustDown -gt 0 -or $m0.AdjustUp -gt 0) {
    Write-Highlight "检测到累积 adjustments (up=$($m0.AdjustUp)/down=$($m0.AdjustDown)) → 进程未重启，currentLimit=$($m0.CurrentLimit) 延续自上次运行。如需从 max_limit 重新开始：停掉 blind-llm-eyes.exe → 重新启动 → 再跑本脚本。" Yellow
}

# 每次脚本运行使用不同的随机色种，避免跨运行 cache 命中
$runOffset = Get-Random -Minimum 1000 -Maximum 999999

Write-Section "Step 1 / 主循环（开始发请求并观察 limit 变化）"

$report = New-Object System.Collections.Generic.List[object]
# 初始状态记 1 条
$report.Add([pscustomobject]@{
    Round = 0
    Timestamp = (Get-Date -Format "HH:mm:ss.fff")
    Limit = $m0.CurrentLimit
    P90Seconds = $m0.P90Seconds
    AdjUp = $m0.AdjustUp; AdjDown = $m0.AdjustDown; AdjNone = $m0.AdjustNone
    HttpStatus = ""; VisionCalls = 0
    Preview = ""
})

for ($r = 1; $r -le $Iterations; $r++) {
    Write-Host ""
    Write-Host "--- Round $r / $Iterations ---------------------------------------------------" -ForegroundColor DarkGray

    $body = New-ImageRequestBody -ImageCount $ImagesPerRequest -ColorSeed ($runOffset + $r)
    Write-Highlight "发送请求: body ~$($body.Length) bytes，含 $ImagesPerRequest 张 distinct PNG"

    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    $resp = Invoke-ImageRequest -Body $body -IterationId $r
    $sw.Stop()
    Write-Highlight "响应: HTTP $($resp.Status) | 本请求 wall ~$([int]$sw.ElapsedMilliseconds) ms"
    if ($resp.Preview) { Write-Host "    preview: $($resp.Preview)" -ForegroundColor Gray }

    Start-Sleep -Milliseconds $SleepBetweenIterationsMs
    $m = Get-AdaptiveMetrics

    $row = [pscustomobject]@{
        Round = $r
        Timestamp = (Get-Date -Format "HH:mm:ss.fff")
        Limit = if ($m) { $m.CurrentLimit } else { $null }
        P90Seconds = if ($m) { $m.P90Seconds } else { $null }
        AdjUp = if ($m) { $m.AdjustUp } else { 0 }
        AdjDown = if ($m) { $m.AdjustDown } else { 0 }
        AdjNone = if ($m) { $m.AdjustNone } else { 0 }
        HttpStatus = $resp.Status
        VisionCalls = $ImagesPerRequest
        Preview = $resp.Preview
    }
    $report.Add($row)

    $color = if (-not $m) { [ConsoleColor]::Red }
             elseif ($report.Count -ge 2 -and $m.CurrentLimit -lt $report[$report.Count - 2].Limit) { [ConsoleColor]::Magenta }
             elseif ($report.Count -ge 2 -and $m.CurrentLimit -gt $report[$report.Count - 2].Limit) { [ConsoleColor]::Green }
             else { [ConsoleColor]::Cyan }
    Write-Highlight ("Round {0} 末状态: limit={1}, P90={2:N2}s, adj(up/dn/no)=({3}/{4}/{5})" -f `
        $r, $row.Limit, $row.P90Seconds, $row.AdjUp, $row.AdjDown, $row.AdjNone) $color
}

Write-Section "Step 2 / 结果总览"

$tbl = $report | Format-Table -AutoSize `
    Round, Timestamp, Limit, @{Name="P90_s";Expression={if ($_.P90Seconds){[math]::Round($_.P90Seconds,2)}else{"n/a"}}}, `
    @{Name="Up";Expression={$_.AdjUp}}, @{Name="Down";Expression={$_.AdjDown}}, @{Name="None";Expression={$_.AdjNone}}, `
    HttpStatus, VisionCalls | Out-String
Write-Host $tbl

Write-Host "CSV 复制（粘贴到 Excel）：" -ForegroundColor DarkYellow
$report | ConvertTo-Csv -NoTypeInformation

Write-Section "如何判断成功"

$last = $report[$report.Count - 1]
$first = $report[0]
if ($null -ne $last.Limit -and $null -ne $first.Limit -and $last.Limit -lt $first.Limit) {
    Write-Host "  [PASS]  ✅ Limit 从 $($first.Limit) 降到了 $($last.Limit)，AI×MD 的 DECREASE 分支生效！" -ForegroundColor Green
    Write-Host "         原因：现实 MiMo 调用时延 ($(if ($last.P90Seconds){[math]::Round($last.P90Seconds,2)}else{"?"})s) > slow_threshold_ms (3s)，每轮都判定 tooSlow。" -ForegroundColor Green
} elseif ($first.Limit -le 2 -and $first.AdjDown -gt 0) {
    Write-Host "  [SKIP]  ⚠️  进程未重启，初始 limit 已在上次运行中降到 $($first.Limit) (=min_limit floor)。" -ForegroundColor Yellow
    Write-Host "         系统行为正确（clamp 生效），但本次运行无法再演示 DECREASE。" -ForegroundColor Yellow
    Write-Host "         如需重新演示：停掉 blind-llm-eyes.exe → 重新启动 → 再跑本脚本。" -ForegroundColor Yellow
} else {
    Write-Host "  [NOTE]  ⚠️  Limit 未下降，检查：" -ForegroundColor Yellow
    Write-Host "    1) config.yaml 是否已按 [RECOMMENDED CONFIG] 粘贴并重启进程？" -ForegroundColor Yellow
    Write-Host "    2) /metrics 是否返回 blind_llm_eyes_adaptive_concurrency_current？(enabled=true 才会有)" -ForegroundColor Yellow
    Write-Host "    3) sample_window 是否足够小？建议 4，脚本每轮正好是 4 张 distinct 图。" -ForegroundColor Yellow
    Write-Host "    4) 是否 sleep 时间 < cooldown_ms，导致下一轮未进入 evaluate？(脚本默认 sleep=1.5s > cooldown=1s，OK)" -ForegroundColor Yellow
}
Write-Host "  [INFO]  如要演示 INCREASE，需要在本地 mock 快 vision（< fast_threshold），" -ForegroundColor Gray
Write-Host "          对应 Go 单测 TestAdaptive_Increase；真实 MiMo 时延高，无法自然触发。" -ForegroundColor Gray
