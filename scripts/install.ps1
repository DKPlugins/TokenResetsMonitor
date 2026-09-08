#Requires -Version 5.1
#Requires -RunAsAdministrator
[CmdletBinding()]
param(
    [ValidatePattern('^v\d+\.\d+\.\d+(-[0-9A-Za-z][0-9A-Za-z.-]*)?$')]
    [string]$Version = 'v1.0.0',
    [string]$InstallDir = (Join-Path $env:ProgramFiles 'TokenResetsMonitor'),
    [string]$DataDir = (Join-Path $env:ProgramData 'TokenResetsMonitor'),
    [switch]$NoStart
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
if (-not [Environment]::Is64BitOperatingSystem -or $env:PROCESSOR_ARCHITECTURE -eq 'ARM64') {
    throw 'This installer requires Windows x64.'
}

function Invoke-Monitor {
    param([string]$Path, [string[]]$Arguments)
    & $Path @Arguments
    if ($LASTEXITCODE -ne 0) { throw "Monitor command failed (exit $LASTEXITCODE)." }
}

function Invoke-WithTemporaryEnvironment {
    param([hashtable]$Variables, [scriptblock]$Action)
    $original = @{}
    try {
        foreach ($name in $Variables.Keys) {
            $original[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
            [Environment]::SetEnvironmentVariable($name, $Variables[$name], 'Process')
        }
        & $Action
    } finally {
        foreach ($name in $original.Keys) {
            if ($null -eq $original[$name]) {
                # PowerShell can bind $null to an empty string for .NET calls.
                # Recent .NET versions preserve that as an empty override.
                Remove-Item -LiteralPath ('Env:\' + $name) -ErrorAction SilentlyContinue
            } else {
                [Environment]::SetEnvironmentVariable($name, $original[$name], 'Process')
            }
        }
    }
}

function Set-PrivateDirectoryAcl {
    param([string]$Path, [System.Security.AccessControl.FileSystemRights]$ServiceRights)
    $acl = New-Object System.Security.AccessControl.DirectorySecurity
    $acl.SetAccessRuleProtection($true, $false)
    foreach ($entry in @(@('S-1-5-18', 'FullControl'), @('S-1-5-32-544', 'FullControl'), @('S-1-5-19', $ServiceRights))) {
        $sid = New-Object System.Security.Principal.SecurityIdentifier($entry[0])
        $rule = New-Object System.Security.AccessControl.FileSystemAccessRule($sid, $entry[1], 'ContainerInherit,ObjectInherit', 'None', 'Allow')
        $acl.AddAccessRule($rule)
    }
    Set-Acl -LiteralPath $Path -AclObject $acl
}

function Resolve-InstallDirectory {
    param([string]$Path)
    if (-not [IO.Path]::IsPathRooted($Path)) { throw 'Installation paths must be absolute.' }
    $resolved = [IO.Path]::GetFullPath($Path).TrimEnd([IO.Path]::DirectorySeparatorChar)
    if ($resolved -eq [IO.Path]::GetPathRoot($resolved).TrimEnd('\')) { throw 'Do not use a drive root as an installation directory.' }
    if ((Test-Path -LiteralPath $resolved) -and ((Get-Item -LiteralPath $resolved).Attributes -band [IO.FileAttributes]::ReparsePoint)) {
        throw 'Installation directories must not be junctions or symbolic links.'
    }
    return $resolved
}

$InstallDir = Resolve-InstallDirectory $InstallDir
$DataDir = Resolve-InstallDirectory $DataDir
if ($InstallDir -eq $DataDir -or
    $InstallDir.StartsWith($DataDir + '\', [StringComparison]::OrdinalIgnoreCase) -or
    $DataDir.StartsWith($InstallDir + '\', [StringComparison]::OrdinalIgnoreCase)) {
    throw 'InstallDir and DataDir must be separate directories, neither containing the other.'
}
function Confirm-OwnedDirectory {
    param([string]$Path, [string]$Role)
    if (-not (Test-Path -LiteralPath $Path)) { return }
    if (-not (Get-Item -LiteralPath $Path).PSIsContainer) { throw 'Installation paths must be directories.' }
    $children = @(Get-ChildItem -LiteralPath $Path -Force)
    if ($children.Count -eq 0) { return }
    $marker = Join-Path $Path '.tokenresetsmonitor-install.json'
    if (-not (Test-Path -LiteralPath $marker)) {
        throw 'A selected directory is not empty and has no TokenResetsMonitor installation marker; use a new dedicated directory.'
    }
    try { $record = Get-Content -LiteralPath $marker -Raw | ConvertFrom-Json } catch { throw 'An installation marker is invalid.' }
    if ($record.project -ne 'DKPlugins/TokenResetsMonitor' -or $record.role -ne $Role) {
        throw 'A selected directory belongs to a different installation role.'
    }
}
Confirm-OwnedDirectory $InstallDir 'binary'
Confirm-OwnedDirectory $DataDir 'data'
$executable = Join-Path $InstallDir 'tokenresetsmonitor.exe'
$config = Join-Path $DataDir 'config.yaml'
$stateDir = Join-Path $DataDir 'data'
$logsDir = Join-Path $DataDir 'logs'
$workRoot = [IO.Path]::GetFullPath([IO.Path]::GetTempPath())
$work = Join-Path $workRoot ('tokenresetsmonitor-install-' + [Guid]::NewGuid().ToString('N'))
$backup = $null
New-Item -ItemType Directory -Path $work | Out-Null
try {
    $asset = "tokenresetsmonitor_${Version}_windows_amd64.zip"
    $base = "https://github.com/DKPlugins/TokenResetsMonitor/releases/download/$Version"
    $archive = Join-Path $work $asset
    $checksums = Join-Path $work 'checksums.txt'
    Invoke-WebRequest -UseBasicParsing -Uri "$base/$asset" -OutFile $archive -TimeoutSec 180
    Invoke-WebRequest -UseBasicParsing -Uri "$base/checksums.txt" -OutFile $checksums -TimeoutSec 60
    $matching = @(Get-Content -LiteralPath $checksums | Where-Object { $_ -match ('^[a-fA-F0-9]{64}\s+' + [regex]::Escape($asset) + '$') })
    if ($matching.Count -ne 1) { throw 'Release checksum is missing or ambiguous.' }
    $expected = ($matching[0] -split '\s+')[0]
    if ((Get-FileHash -Algorithm SHA256 -LiteralPath $archive).Hash -ne $expected) { throw 'Checksum verification failed.' }
    $extract = Join-Path $work 'extracted'
    Expand-Archive -LiteralPath $archive -DestinationPath $extract
    $candidate = Join-Path $extract 'tokenresetsmonitor.exe'
    Invoke-Monitor $candidate @('version')
    if (Test-Path -LiteralPath $config) { Invoke-Monitor $candidate @('config', 'validate', '--config', $config) }

    $service = Get-Service -Name TokenResetsMonitor -ErrorAction SilentlyContinue
    if ($null -ne $service) {
        if (-not (Test-Path -LiteralPath $executable)) { throw 'Existing service uses a different installation; supply its InstallDir.' }
        $serviceConfig = Get-CimInstance Win32_Service -Filter "Name='TokenResetsMonitor'"
        $expectedCommand = '"' + $executable + '" run --config "' + $config + '"'
        # SCM may omit quotes when an argument contains no spaces.
        if (($serviceConfig.PathName.Replace('"', '')).ToLowerInvariant() -ne ($expectedCommand.Replace('"', '')).ToLowerInvariant()) {
            throw 'Existing service uses different paths; supply its original InstallDir and DataDir.'
        }
        Invoke-Monitor $executable @('service', 'stop')
    }
    if (Test-Path -LiteralPath $executable) {
        $running = @(Get-CimInstance Win32_Process -Filter "Name='tokenresetsmonitor.exe'" | Where-Object { $_.ExecutablePath -eq $executable })
        if ($running.Count -gt 0) { throw 'Stop foreground monitor processes before updating.' }
    }

    foreach ($directory in @($InstallDir, $DataDir, $stateDir, $logsDir)) {
        New-Item -ItemType Directory -Path $directory -Force | Out-Null
    }
    Set-PrivateDirectoryAcl $InstallDir 'ReadAndExecute'
    Set-PrivateDirectoryAcl $DataDir 'ReadAndExecute'
    Set-PrivateDirectoryAcl $stateDir 'Modify'
    Set-PrivateDirectoryAcl $logsDir 'Modify'
    foreach ($entry in @(@($InstallDir, 'binary'), @($DataDir, 'data'))) {
        $markerData = @{ project = 'DKPlugins/TokenResetsMonitor'; role = $entry[1] } | ConvertTo-Json
        Set-Content -LiteralPath (Join-Path $entry[0] '.tokenresetsmonitor-install.json') -Value $markerData -Encoding UTF8
    }

    if ((Test-Path -LiteralPath $executable) -or (Test-Path -LiteralPath $config)) {
        $backup = Join-Path $DataDir ('backups\' + [DateTime]::UtcNow.ToString('yyyyMMddTHHmmssZ') + '-' + [Guid]::NewGuid().ToString('N').Substring(0, 8))
        New-Item -ItemType Directory -Path $backup -Force | Out-Null
        if (Test-Path -LiteralPath $executable) { Copy-Item -LiteralPath $executable -Destination $backup }
        if (Test-Path -LiteralPath $config) { Copy-Item -LiteralPath $config -Destination $backup }
        Copy-Item -LiteralPath $stateDir -Destination (Join-Path $backup 'data') -Recurse
        Write-Host "Backup: $backup"
    }
    Copy-Item -LiteralPath $candidate -Destination $executable -Force
    if (-not (Test-Path -LiteralPath $config)) {
        $initialEnvironment = @{
            TRM_STATE_PATH = (Join-Path $stateDir 'state.db')
            TRM_LOGGING_FORMAT = 'json'
            TRM_LOGGING_FILE_ENABLED = 'true'
            TRM_LOGGING_DIRECTORY = $logsDir
        }
        Invoke-WithTemporaryEnvironment $initialEnvironment {
            Invoke-Monitor $executable @('init', '--defaults', '--config', $config)
        }
    }
    Invoke-Monitor $executable @('config', 'validate', '--config', $config)
    if ($null -eq $service) { Invoke-Monitor $executable @('service', 'install', '--config', $config) }
    if (-not $NoStart) {
        Invoke-Monitor $executable @('service', 'start')
        Start-Sleep -Seconds 2
        if ((Get-Service -Name TokenResetsMonitor).Status -ne 'Running') { throw 'Service failed to start; inspect Windows Event Viewer and application logs.' }
    }
    Write-Host "Installed $Version. Configure $config and restart the service to apply changes."
    Write-Host 'Both notification channels are disabled in a new default configuration.'
} catch {
    if ($null -ne $backup) { Write-Warning "Installation failed. Backup retained at $backup. Check service state before restoring." }
    throw
} finally {
    # Verify the actual absolute target stays under the explicitly named temp root.
    $resolvedWork = [IO.Path]::GetFullPath($work)
    $allowedPrefix = $workRoot.TrimEnd('\') + '\tokenresetsmonitor-install-'
    if ($resolvedWork.StartsWith($allowedPrefix, [StringComparison]::OrdinalIgnoreCase) -and (Test-Path -LiteralPath $resolvedWork)) {
        Remove-Item -LiteralPath $resolvedWork -Recurse -Force
    }
}
