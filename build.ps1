$ErrorActionPreference = 'Stop'
Set-Location $PSScriptRoot
New-Item -ItemType Directory -Force dist | Out-Null
# despeje.exe: consola (para -check y pruebas). despejew.exe: sin ventana (tarea programada).
go build -trimpath -ldflags "" -o dist\despeje.exe .
go build -trimpath -ldflags "-H=windowsgui" -o dist\despejew.exe .
Write-Host "OK: dist\despeje.exe y dist\despejew.exe"
