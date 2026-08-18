# pre-commit hook (PowerShell): block hardcoded API keys from entering git history
# Matches common key prefixes: sk- (OpenAI/DeepSeek/Anthropic), AKIA (AWS), etc.
# Install: git config core.hooksPath .githooks
# If hooksPath doesn't support .ps1, copy this file to .git/hooks/pre-commit

$ErrorActionPreference = "Stop"

# Get staged files (added/modified, excluding deletions)
$files = git diff --cached --name-only --diff-filter=ACM 2>$null | Where-Object { $_ -notmatch '\.git$' }

if (-not $files) {
    exit 0
}

# Pattern for common API key families
$pattern = '(sk-[A-Za-z0-9]{20,}|AKIA[A-Z0-9]{16}|ghp_[A-Za-z0-9]{36}|github_pat_[A-Za-z0-9_]{20,}|AIza[A-Za-z0-9_-]{30,}|xox[baprs]-[A-Za-z0-9-]{10,}|eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}|-----BEGIN [A-Z ]*PRIVATE KEY-----)'

$found = ""
foreach ($file in $files) {
    if (-not (Test-Path $file -PathType Leaf)) {
        continue
    }
    # Skip binary files by checking content readability
    try {
        $content = Get-Content -Path $file -Raw -ErrorAction Stop
    } catch {
        continue
    }
    if ($content -match $pattern) {
        $matches = Select-String -Path $file -Pattern $pattern -AllMatches
        foreach ($m in $matches) {
            $found += "`n$file:$($m.LineNumber): $($m.Line.Trim())`n"
        }
    }
}

if ($found) {
    Write-Host ""
    Write-Host "================================================"
    Write-Host "  COMMIT BLOCKED: hardcoded API key detected"
    Write-Host "================================================"
    Write-Host ""
    Write-Host "The following files contain疑似 API key strings:"
    Write-Host $found
    Write-Host "Use environment variables instead of hardcoding:"
    Write-Host "  Python:  os.environ.get(`"API_KEY`")"
    Write-Host "  Go:      os.Getenv(`"API_KEY`")"
    Write-Host "  YAML:    store real keys in config.yaml (gitignored)"
    Write-Host ""
    Write-Host "If confirmed as false positive (e.g. test placeholder sk-xxx), skip with:"
    Write-Host "  git commit --no-verify"
    Write-Host ""
    exit 1
}

exit 0
