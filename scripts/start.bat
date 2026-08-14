@echo off
REM Double-click launcher for blind-llm-eyes on Windows.
REM Runs the proxy server; window stays open after exit so error messages
REM are visible. Place next to blind-llm-eyes.exe in the release archive.

cd /d "%~dp0"

echo === blind-llm-eyes starting ===
echo.

blind-llm-eyes.exe start

echo.
echo === blind-llm-eyes exited (code %errorlevel%) ===
echo.
pause
