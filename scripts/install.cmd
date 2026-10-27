@echo off
rem Instalador de Astrea para Windows: doble clic. Pide permisos de admin (UAC)
rem una vez y no depende de la ExecutionPolicy. Usa install.ps1 de esta misma
rem carpeta si existe; si no, descarga la ultima version.
rem
rem Despliegue en lote (USB, carpeta compartida, GPO/Intune/PDQ): pon junto a
rem este archivo un install.env con las variables (ver install.env.example) y,
rem si quieres instalar sin Internet, astrea-agent.exe.
setlocal
set "ASTREA_SCRIPT=%~dp0install.ps1"
if exist "%ASTREA_SCRIPT%" (
  powershell.exe -NoProfile -ExecutionPolicy Bypass -Command "iex ([IO.File]::ReadAllText($env:ASTREA_SCRIPT, [Text.Encoding]::UTF8))"
) else (
  set "ASTREA_SCRIPT="
  powershell.exe -NoProfile -ExecutionPolicy Bypass -Command "[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor 3072; iex (irm https://raw.githubusercontent.com/Solar2004/Astrea/main/scripts/install.ps1)"
)
set "RC=%ERRORLEVEL%"
rem Como SYSTEM (script de arranque) no hay nadie para pulsar una tecla: un
rem pause ahi deja el arranque de Windows esperando.
if /i not "%USERNAME%"=="%COMPUTERNAME%$" (
  echo.
  pause
)
exit /b %RC%
