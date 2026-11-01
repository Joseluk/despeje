# despeje - desinstalador para Windows:
#
#   irm https://raw.githubusercontent.com/Joseluk/despeje/main/uninstall.ps1 | iex
#
# Quita la tarea programada, restaura el proxy de Windows y borra el programa.
& {
$ErrorActionPreference = 'Stop'
$Raw  = 'https://raw.githubusercontent.com/Joseluk/despeje/main'
$Dest = Join-Path $env:USERPROFILE '.despeje'

if (Get-ScheduledTask -TaskName 'despeje' -ErrorAction SilentlyContinue) {
    Stop-ScheduledTask -TaskName 'despeje'
    Unregister-ScheduledTask -TaskName 'despeje' -Confirm:$false
}
Get-Process despejew, despeje -ErrorAction SilentlyContinue | Stop-Process -Force
Start-Sleep -Milliseconds 500
if (Test-Path "$Dest\despeje.exe") {
    & "$Dest\despeje.exe" -sysproxy-off -config "$Dest\config.json"
}
Remove-Item -Recurse -Force $Dest -ErrorAction SilentlyContinue

Write-Host '==> despeje desinstalado de este equipo.' -ForegroundColor Green
Write-Host '    Para quitar tambien el usuario del VPS, ejecuta alli:'
Write-Host "    curl -fsSL $Raw/setup-vps.sh | sudo bash -s -- --remove"
Write-Host '    (La clave SSH de despeje se ha borrado con el programa; al reinstalar se crea otra.)'
}
