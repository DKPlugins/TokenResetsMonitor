#Requires -Version 5.1
#Requires -RunAsAdministrator
# Disposable CI runner acceptance check; do not run on an existing installation.
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
if ($env:CI -ne 'true') { throw 'Run this lifecycle test only on a disposable CI runner (CI=true).' }
if (Get-Service -Name TokenResetsMonitor -ErrorAction SilentlyContinue) { throw 'A TokenResetsMonitor service already exists.' }
$tempRoot = [IO.Path]::GetFullPath([IO.Path]::GetTempPath())
$testRoot = Join-Path $tempRoot ('TokenResetsMonitor service test ' + [Guid]::NewGuid().ToString('N'))
$config = Join-Path $testRoot 'config.yaml'
$binary = Join-Path $testRoot 'tokenresetsmonitor.exe'
$installed = $false
$oldEnvironment = @{}
function Invoke-TestMonitor {
    param([string[]]$Arguments)
    & $binary @Arguments
    if ($LASTEXITCODE -ne 0) { throw "Monitor command failed: $($Arguments[0]) (exit $LASTEXITCODE)." }
}
try {
    New-Item -ItemType Directory -Path $testRoot | Out-Null
    Copy-Item -LiteralPath (Join-Path $PWD 'tokenresetsmonitor.exe') -Destination $binary
    $acl = Get-Acl -LiteralPath $testRoot
    $sid = New-Object System.Security.Principal.SecurityIdentifier('S-1-5-19')
    $rule = New-Object System.Security.AccessControl.FileSystemAccessRule($sid, 'Modify', 'ContainerInherit,ObjectInherit', 'None', 'Allow')
    $acl.AddAccessRule($rule)
    Set-Acl -LiteralPath $testRoot -AclObject $acl
    $settings = @{
        TRM_STATE_PATH = (Join-Path $testRoot 'state.db')
        TRM_API_BASE_URL = 'http://127.0.0.1:1/api/v1'
        TRM_LOGGING_FILE_ENABLED = 'true'
        TRM_LOGGING_FORMAT = 'json'
        TRM_LOGGING_DIRECTORY = (Join-Path $testRoot 'logs')
    }
    foreach ($key in $settings.Keys) {
        $oldEnvironment[$key] = [Environment]::GetEnvironmentVariable($key, 'Process')
        [Environment]::SetEnvironmentVariable($key, $settings[$key], 'Process')
    }
    Invoke-TestMonitor @('init', '--defaults', '--config', $config)
    Invoke-TestMonitor @('service', 'install', '--config', $config)
    $installed = $true
    $service = Get-CimInstance Win32_Service -Filter "Name='TokenResetsMonitor'"
    if ($service.StartName -ne 'NT AUTHORITY\LocalService') { throw 'Unexpected service account.' }
    if ($service.StartMode -ne 'Auto') { throw 'Service is not configured to start automatically.' }
    Invoke-TestMonitor @('service', 'start')
    Start-Sleep -Seconds 2
    if ((Get-Service TokenResetsMonitor).Status -ne 'Running') { throw 'Service stopped unexpectedly.' }
    Invoke-TestMonitor @('service', 'status')
    Invoke-TestMonitor @('service', 'stop')
    if (-not (Test-Path -LiteralPath (Join-Path $testRoot 'state.db'))) { throw 'Service did not create its state database.' }
    # Replacing the stopped executable models the installer update operation.
    Copy-Item -LiteralPath (Join-Path $PWD 'tokenresetsmonitor.exe') -Destination $binary -Force
    Invoke-TestMonitor @('service', 'start')
    Invoke-TestMonitor @('service', 'stop')
    Invoke-TestMonitor @('service', 'uninstall')
    $installed = $false
    if (-not (Test-Path -LiteralPath $config)) { throw 'Uninstall deleted configuration.' }
    if (-not (Test-Path -LiteralPath (Join-Path $testRoot 'state.db'))) { throw 'Uninstall deleted state.' }
    Write-Host 'Windows service install/start/stop/update/uninstall passed.'
} finally {
    if ($installed) {
        & $binary service stop
        & $binary service uninstall
    }
    foreach ($key in $oldEnvironment.Keys) { [Environment]::SetEnvironmentVariable($key, $oldEnvironment[$key], 'Process') }
    $resolved = [IO.Path]::GetFullPath($testRoot)
    $allowed = $tempRoot.TrimEnd('\') + '\TokenResetsMonitor service test '
    if ($resolved.StartsWith($allowed, [StringComparison]::OrdinalIgnoreCase) -and (Test-Path -LiteralPath $resolved)) {
        Remove-Item -LiteralPath $resolved -Recurse -Force
    }
}
