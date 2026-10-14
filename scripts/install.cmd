@echo off
rem Instalador de Astrea para Windows: doble clic. Pide permisos de admin (UAC)
rem una vez y no depende de la ExecutionPolicy. Usa install.ps1 de esta misma
rem carpeta si existe; si no, descarga la ultima version.
setlocal
set "ASTREA_SCRIPT=%~dp0install.ps1"
if exist "%ASTREA_SCRIPT%" (
  powershell.exe -NoProfile -ExecutionPolicy Bypass -Command "iex ([IO.File]::ReadAllText($env:ASTREA_SCRIPT, [Text.Encoding]::UTF8))"
) else (
  set "ASTREA_SCRIPT="
  powershell.exe -NoProfile -ExecutionPolicy Bypass -Command "[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor 3072; iex (irm https://raw.githubusercontent.com/Solar2004/Astrea/main/scripts/install.ps1)"
)
echo.
pause
