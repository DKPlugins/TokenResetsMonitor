#Requires -Version 5.1
#Requires -RunAsAdministrator
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$Installer,
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^v\d+\.\d+\.\d+(-[0-9A-Za-z][0-9A-Za-z.-]*)?$')]
    [string]$Version
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
if ($env:CI -ne 'true') { throw 'Only run on a disposable CI runner (CI=true).' }
if (-not (Test-Path -LiteralPath $Installer)) { throw 'Published installer was not downloaded.' }
if (Get-Service -Name TokenResetsMonitor -ErrorAction SilentlyContinue) { throw 'Run this negative test before installing TokenResetsMonitor.' }
$tempRoot = [IO.Path]::GetFullPath([IO.Path]::GetTempPath())
$testRoot = Join-Path $tempRoot ('TokenResetsMonitor checksum test ' + [Guid]::NewGuid().ToString('N'))
$binaryDir = Join-Path $testRoot 'binary'
$dataDir = Join-Path $testRoot 'data'
$rejected = $false
New-Item -ItemType Directory -Path $testRoot | Out-Null

# PowerShell's command lookup resolves this wrapper in the called script too.
# The real HTTPS downloads are unchanged; only archive bytes are corrupted.
function Invoke-WebRequest {
    [CmdletBinding()]
    param([switch]$UseBasicParsing, [string]$Uri, [string]$OutFile, [int]$TimeoutSec)
    Microsoft.PowerShell.Utility\Invoke-WebRequest -UseBasicParsing:$UseBasicParsing -Uri $Uri -OutFile $OutFile -TimeoutSec $TimeoutSec
    if ($OutFile.EndsWith('.zip', [StringComparison]::OrdinalIgnoreCase)) {
        $archive = [IO.File]::Open($OutFile, [IO.FileMode]::Append, [IO.FileAccess]::Write)
        try { $archive.WriteByte(0) } finally { $archive.Dispose() }
    }
}

try {
    try {
        & $Installer -Version $Version -InstallDir $binaryDir -DataDir $dataDir -NoStart
    } catch {
        if ($_.Exception.Message -ne 'Checksum verification failed.') { throw }
        $rejected = $true
    }
    if (-not $rejected) { throw 'Installer accepted a corrupted archive.' }
    if ((Test-Path -LiteralPath $binaryDir) -or (Test-Path -LiteralPath $dataDir)) {
        throw 'Installer changed installation directories before rejecting the archive.'
    }
    if (Get-Service -Name TokenResetsMonitor -ErrorAction SilentlyContinue) {
        throw 'Installer registered a service before rejecting the archive.'
    }
    Write-Host 'Published Windows installer rejected a corrupted archive before installation.'
} finally {
    # On an unexpected installation, preserve the directory for CI diagnostics.
    $resolved = [IO.Path]::GetFullPath($testRoot)
    $allowed = $tempRoot.TrimEnd('\') + '\TokenResetsMonitor checksum test '
    if ($rejected -and $resolved.StartsWith($allowed, [StringComparison]::OrdinalIgnoreCase) -and (Test-Path -LiteralPath $resolved)) {
        Remove-Item -LiteralPath $resolved -Recurse -Force
    }
}
