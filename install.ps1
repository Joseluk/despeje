# despeje - instalador para Windows (no necesita administrador):
#
#   irm https://raw.githubusercontent.com/Joseluk/despeje/main/install.ps1 | iex
#
# Descarga el programa, crea una clave SSH, prepara tu VPS por SSH (usuario
# que solo puede abrir tuneles), lo comprueba y lo deja arrancando al iniciar
# sesion. Volver a ejecutarlo actualiza el programa.
#
# Para instalar sin preguntas, define antes:
#   $env:DESPEJE_VPS  = 'ubuntu@203.0.113.10'   # usuario con sudo @ host[:puerto]
#   $env:DESPEJE_ISP  = 'movistar'              # any|movistar|vodafone|orange|digi|masmovil
#   $env:DESPEJE_MODE = 'auto'                  # auto|cdn|all
# Otras: DESPEJE_SKIP_VPS=1 (el VPS ya esta preparado con setup-vps.sh),
#        DESPEJE_VERSION=v1.2.3, DESPEJE_LOCAL_DIST=<carpeta con despeje.exe y despejew.exe>.
& {
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

$Repo = 'Joseluk/despeje'
$Raw  = "https://raw.githubusercontent.com/$Repo/main"
# Fuera de AppData: las apps MSIX virtualizan AppData y el Programador de
# tareas no veria los ficheros.
$Dest = Join-Path $env:USERPROFILE '.despeje'
$Cfg  = Join-Path $Dest 'config.json'
$Exe  = Join-Path $Dest 'despeje.exe'
# Clave y known_hosts propios: no dependemos del estado de ~/.ssh (permisos rotos,
# ficheros de otras herramientas...). Con '~' ssh.exe no se lia con espacios.
$KeyRel   = '~/.despeje/id_ed25519'
$KnownRel = '~/.despeje/known_hosts'
$Isps  = 'any', 'movistar', 'vodafone', 'orange', 'digi', 'masmovil'
$Modes = 'auto', 'cdn', 'all'

function Step($msg) { Write-Host "==> $msg" -ForegroundColor Green }
function Ask($question, $default) {
    $a = Read-Host "$question [$default]"
    if ([string]::IsNullOrWhiteSpace($a)) { return $default }
    return $a.Trim()
}

Write-Host ''
Write-Host 'despeje: saca por tu VPS solo las IPs que LaLiga bloquea' -ForegroundColor Cyan
Write-Host ''
$principal = [Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()
if ($principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Write-Host '!! Estas en un PowerShell de administrador. No hace falta y puede dejar ficheros con' -ForegroundColor Yellow
    Write-Host '   permisos raros; si algo falla, repitelo desde un PowerShell normal.' -ForegroundColor Yellow
    Write-Host ''
}

# --- 1. Configuracion ---------------------------------------------------------
$existing = $null
if (Test-Path $Cfg) { $existing = [IO.File]::ReadAllText($Cfg) | ConvertFrom-Json }
$reconfigure = $true
if ($existing -and -not $env:DESPEJE_VPS) {
    $a = Ask "Ya esta instalado (VPS $($existing.vps.host)). Solo actualizar el programa? (S/n)" 'S'
    $reconfigure = $a -match '^[nN]'
}

if ($reconfigure) {
    $vps = $env:DESPEJE_VPS
    if (-not $vps) {
        Write-Host 'Necesito un usuario con sudo en tu VPS (solo se usa ahora, para prepararlo).'
        $vps = Read-Host 'usuario@host[:puerto] (ej. ubuntu@203.0.113.10)'
    }
    if ($vps.Trim() -notmatch '^(?<u>[^@\s]+)@(?<h>\[[^\]]+\]|[^:\s]+)(:(?<p>\d+))?$') {
        throw "Formato esperado: usuario@host[:puerto]  (recibido: '$vps')"
    }
    $adminUser = $Matches.u
    $vpsHost = $Matches.h.Trim('[', ']')
    $port = if ($Matches.p) { [int]$Matches.p } else { 22 }

    $isp = if ($env:DESPEJE_ISP) { $env:DESPEJE_ISP } else { Ask "Tu operador ($($Isps -join ', '))" 'any' }
    $isp = $isp.ToLower()
    if ($Isps -notcontains $isp) { throw "Operador desconocido: $isp" }

    if ($env:DESPEJE_MODE) { $mode = $env:DESPEJE_MODE } else {
        Write-Host 'Modo: auto = solo IPs bloqueadas ahora (recomendado), cdn = todo Cloudflare siempre, all = todo el trafico'
        $mode = Ask 'Modo' 'auto'
    }
    $mode = $mode.ToLower()
    if ($Modes -notcontains $mode) { throw "Modo desconocido: $mode" }
}

# --- 2. Programa ----------------------------------------------------------------
$task = Get-ScheduledTask -TaskName 'despeje' -ErrorAction SilentlyContinue
if ($task) { $task | Stop-ScheduledTask }
Get-Process despejew, despeje -ErrorAction SilentlyContinue | Stop-Process -Force
New-Item -ItemType Directory -Force $Dest | Out-Null

if ($env:DESPEJE_LOCAL_DIST) {
    Step "Copiando binarios de $env:DESPEJE_LOCAL_DIST"
    Copy-Item (Join-Path $env:DESPEJE_LOCAL_DIST 'despeje.exe'), (Join-Path $env:DESPEJE_LOCAL_DIST 'despejew.exe') $Dest -Force
} else {
    $arch = 'amd64'
    if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64' -or $env:PROCESSOR_ARCHITEW6432 -eq 'ARM64') { $arch = 'arm64' }
    $base = if ($env:DESPEJE_VERSION) { "https://github.com/$Repo/releases/download/$env:DESPEJE_VERSION" } else { "https://github.com/$Repo/releases/latest/download" }
    Step "Descargando despeje (windows/$arch)"
    $tmp = Join-Path $env:TEMP ("despeje-" + [guid]::NewGuid())
    New-Item -ItemType Directory $tmp | Out-Null
    try {
        Invoke-WebRequest -UseBasicParsing "$base/SHA256SUMS" -OutFile "$tmp\SHA256SUMS"
        $sums = @{}
        foreach ($line in Get-Content "$tmp\SHA256SUMS") {
            $parts = $line -split '\s+\*?'
            if ($parts.Count -ge 2) { $sums[$parts[1]] = $parts[0].ToLower() }
        }
        foreach ($name in 'despeje', 'despejew') {
            $file = "$name-windows-$arch.exe"
            Invoke-WebRequest -UseBasicParsing "$base/$file" -OutFile "$tmp\$file"
            $got = (Get-FileHash "$tmp\$file" -Algorithm SHA256).Hash.ToLower()
            if ($got -ne $sums[$file]) { throw "El checksum de $file no coincide; descarga corrupta o manipulada" }
            Copy-Item "$tmp\$file" (Join-Path $Dest "$name.exe") -Force
        }
    } finally {
        Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
    }
}
Get-ChildItem $Dest -Filter *.exe | Unblock-File
Step "$(& $Exe -version) instalado en $Dest"

# --- 3. Clave SSH y VPS ----------------------------------------------------------
if ($reconfigure) {
    $pub = & $Exe -keygen $KeyRel
    if ($LASTEXITCODE -ne 0) { throw 'No se pudo crear la clave SSH' }
    $pub = "$pub".Trim()
    $setupCmd = "curl -fsSL $Raw/setup-vps.sh | sudo bash -s -- '$pub'"

    if (-not (Get-Command ssh.exe -ErrorAction SilentlyContinue)) {
        throw 'Falta el cliente OpenSSH de Windows. Instalalo desde PowerShell como administrador con: Add-WindowsCapability -Online -Name OpenSSH.Client~~~~0.0.1.0'
    }
    if ($env:DESPEJE_SKIP_VPS -eq '1') {
        Step 'Saltando la preparacion del VPS (DESPEJE_SKIP_VPS=1). Si no lo has hecho, ejecuta en el VPS:'
        Write-Host "    $setupCmd"
    } else {
        Step "Preparando el VPS $adminUser@$vpsHost (puede pedirte la clave de SSH y la de sudo)"
        # Sin comillas dobles: Windows PowerShell 5.1 las rompe al pasar argumentos a ssh.exe.
        $remote = "S=sudo; [ `$(id -u) -eq 0 ] && S=; (curl -fsSL $Raw/setup-vps.sh || wget -qO- $Raw/setup-vps.sh) | `$S bash -s -- '$pub'"
        & ssh.exe -t -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=$KnownRel -p $port "$adminUser@$vpsHost" $remote
        if ($LASTEXITCODE -ne 0) {
            throw "No se pudo preparar el VPS. Hazlo a mano ejecutando alli:`n    $setupCmd`ny repite la instalacion con `$env:DESPEJE_SKIP_VPS='1'"
        }
    }
    # Guarda la huella del VPS en known_hosts si aun no estaba (despeje la verifica).
    # En PowerShell 5.1, 2>$null con 'Stop' convierte cualquier aviso de ssh en error.
    $ErrorActionPreference = 'Continue'
    & ssh.exe -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=$KnownRel -o BatchMode=yes -o ConnectTimeout=10 -i "$env:USERPROFILE\.despeje\id_ed25519" -p $port "despeje@$vpsHost" exit 2>$null | Out-Null
    $ErrorActionPreference = 'Stop'

    $conf = [ordered]@{
        listen         = if ($existing.listen) { $existing.listen } else { '127.0.0.1:9119' }
        mode           = $mode
        isp            = $isp
        refresh        = '2m'
        direct_timeout = '4s'
        fallback       = $true
        system_proxy   = $true
        via_vps        = @($existing.via_vps | Where-Object { $_ })
        direct         = @($existing.direct | Where-Object { $_ })
        log_file       = '~/.despeje/despeje.log'
        vps            = [ordered]@{ host = $vpsHost; port = $port; user = 'despeje'; key = $KeyRel; known_hosts = $KnownRel }
    }
    [IO.File]::WriteAllText($Cfg, ($conf | ConvertTo-Json -Depth 4))
}

# --- 4. Comprobacion ------------------------------------------------------------
Step 'Comprobando'
& $Exe -check -config $Cfg
if ($LASTEXITCODE -ne 0) { throw "La comprobacion ha fallado. Revisa $Cfg y vuelve a ejecutar el instalador." }

# --- 5. Arranque al iniciar sesion ----------------------------------------------
$action = New-ScheduledTaskAction -Execute (Join-Path $Dest 'despejew.exe') -Argument "-config `"$Cfg`"" -WorkingDirectory $Dest
$trigger = New-ScheduledTaskTrigger -AtLogOn -User "$env:USERDOMAIN\$env:USERNAME"
$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
    -ExecutionTimeLimit ([TimeSpan]::Zero) -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) `
    -MultipleInstances IgnoreNew
Register-ScheduledTask -TaskName 'despeje' -Action $action -Trigger $trigger -Settings $settings `
    -Description 'despeje: saca por tu VPS las IPs bloqueadas por LaLiga' -Force | Out-Null
Start-ScheduledTask -TaskName 'despeje'

$listen = ([IO.File]::ReadAllText($Cfg) | ConvertFrom-Json).listen -replace '^0\.0\.0\.0:', '127.0.0.1:'
$ok = $false
foreach ($i in 1..10) {
    Start-Sleep -Seconds 1
    try { $ok = (Invoke-RestMethod "http://$listen/status" -TimeoutSec 2).vps_conectado; if ($ok) { break } } catch {}
}
Write-Host ''
if ($ok) { Step 'Listo: despeje esta funcionando y arrancara solo al iniciar sesion.' }
else { Write-Host "!! La tarea esta creada pero aun no responde; mira el log en $Dest\despeje.log" -ForegroundColor Yellow }
Write-Host "   Estado:      http://$listen/status"
Write-Host "   Log:         $Dest\despeje.log"
Write-Host "   Desinstalar: irm $Raw/uninstall.ps1 | iex"
Write-Host ''
}
