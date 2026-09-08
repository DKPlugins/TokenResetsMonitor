#Requires -Version 5.1
[CmdletBinding()]
param([string]$Binary = (Join-Path (Split-Path $PSScriptRoot -Parent) 'tokenresetsmonitor.exe'))
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
if (-not (Test-Path -LiteralPath $Binary)) { throw 'Build tokenresetsmonitor.exe before running this test.' }
$Binary = [IO.Path]::GetFullPath($Binary)

# Load only the production helper's AST. The installer itself is never invoked:
# this regression needs no administrator privileges, downloads, or services.
$tokens = $null
$parseErrors = $null
$ast = [System.Management.Automation.Language.Parser]::ParseFile((Join-Path $PSScriptRoot 'install.ps1'), [ref]$tokens, [ref]$parseErrors)
if ($parseErrors.Count -gt 0) { throw 'Installer has parsing errors.' }
$helper = $ast.Find({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq 'Invoke-WithTemporaryEnvironment' }, $false)
if ($null -eq $helper) { throw 'Production environment helper is missing.' }
. ([scriptblock]::Create($helper.Extent.Text))
$parser = $ast.Find({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq 'ConvertFrom-ServiceEnvironment' }, $false)
if ($null -eq $parser) { throw 'Production service environment parser is missing.' }
. ([scriptblock]::Create($parser.Extent.Text))
$parsed = ConvertFrom-ServiceEnvironment @('TRM_INSTALLER_TEST_PRESENT=value=with=equals', 'TRM_INSTALLER_TEST_EMPTY=')
if ($parsed.TRM_INSTALLER_TEST_PRESENT -ne 'value=with=equals' -or $parsed.TRM_INSTALLER_TEST_EMPTY -ne '') {
    throw 'Service environment values were not preserved.'
}
$invalidRejected = $false
try { ConvertFrom-ServiceEnvironment @('missing separator') | Out-Null } catch { $invalidRejected = $true }
if (-not $invalidRejected) { throw 'Malformed service environment assignment was accepted.' }

$names = @('TRM_INSTALLER_TEST_ABSENT', 'TRM_INSTALLER_TEST_PRESENT', 'TRM_INSTALLER_TEST_EMPTY',
    'TRM_STATE_PATH', 'TRM_LOGGING_FILE_ENABLED', 'TRM_LOGGING_FORMAT', 'TRM_LOGGING_DIRECTORY')
$saved = @{}
foreach ($name in $names) { $saved[$name] = [Environment]::GetEnvironmentVariable($name, 'Process') }
$tempRoot = [IO.Path]::GetFullPath([IO.Path]::GetTempPath())
$testRoot = Join-Path $tempRoot ('TokenResetsMonitor env test ' + [Guid]::NewGuid().ToString('N'))
try {
    Remove-Item -LiteralPath Env:\TRM_INSTALLER_TEST_ABSENT -ErrorAction SilentlyContinue
    [Environment]::SetEnvironmentVariable('TRM_INSTALLER_TEST_PRESENT', 'caller-value', 'Process')
    $changes = @{ TRM_INSTALLER_TEST_ABSENT = 'temporary'; TRM_INSTALLER_TEST_PRESENT = 'temporary' }
    Invoke-WithTemporaryEnvironment $changes {
        if ($env:TRM_INSTALLER_TEST_ABSENT -ne 'temporary' -or $env:TRM_INSTALLER_TEST_PRESENT -ne 'temporary') {
            throw 'Temporary overrides were not available inside the action.'
        }
    }
    if (Test-Path -LiteralPath Env:\TRM_INSTALLER_TEST_ABSENT) { throw 'An originally absent variable was left in the process environment.' }
    if ($env:TRM_INSTALLER_TEST_PRESENT -ne 'caller-value') { throw 'An original value was not restored.' }

    $failed = $false
    try { Invoke-WithTemporaryEnvironment $changes { throw 'Expected action failure.' } } catch {
        if ($_.Exception.Message -ne 'Expected action failure.') { throw }
        $failed = $true
    }
    if (-not $failed) { throw 'The helper swallowed an action failure.' }
    if (Test-Path -LiteralPath Env:\TRM_INSTALLER_TEST_ABSENT) { throw 'Failure cleanup left an empty variable behind.' }
    if ($env:TRM_INSTALLER_TEST_PRESENT -ne 'caller-value') { throw 'Failure cleanup lost an original value.' }

    # .NET 9+ permits an explicitly empty environment value; older runtimes
    # remove the variable instead and cannot represent this input condition.
    [Environment]::SetEnvironmentVariable('TRM_INSTALLER_TEST_EMPTY', '', 'Process')
    if (Test-Path -LiteralPath Env:\TRM_INSTALLER_TEST_EMPTY) {
        Invoke-WithTemporaryEnvironment @{ TRM_INSTALLER_TEST_EMPTY = 'temporary' } { }
        if (-not (Test-Path -LiteralPath Env:\TRM_INSTALLER_TEST_EMPTY) -or $env:TRM_INSTALLER_TEST_EMPTY -ne '') {
            throw 'An originally empty value was not preserved.'
        }
    }

    foreach ($name in @('TRM_STATE_PATH', 'TRM_LOGGING_FILE_ENABLED', 'TRM_LOGGING_FORMAT', 'TRM_LOGGING_DIRECTORY')) {
        Remove-Item -LiteralPath ('Env:\' + $name) -ErrorAction SilentlyContinue
    }
    New-Item -ItemType Directory -Path $testRoot | Out-Null
    $config = Join-Path $testRoot 'config.yaml'
    $initial = @{
        TRM_STATE_PATH = (Join-Path $testRoot 'data/state.db')
        TRM_LOGGING_FILE_ENABLED = 'true'
        TRM_LOGGING_FORMAT = 'json'
        TRM_LOGGING_DIRECTORY = (Join-Path $testRoot 'logs')
    }
    Invoke-WithTemporaryEnvironment $initial {
        & $Binary init --defaults --config $config
        if ($LASTEXITCODE -ne 0) { throw 'Initialization with temporary installer settings failed.' }
    }
    foreach ($name in $initial.Keys) {
        if (Test-Path -LiteralPath ('Env:\' + $name)) { throw 'Initialization leaked a temporary environment override.' }
    }
    $aclHelper = $ast.Find({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq 'Set-ServiceConfigurationAcl' }, $false)
    if ($null -eq $aclHelper) { throw 'Production service configuration ACL helper is missing.' }
    . ([scriptblock]::Create($aclHelper.Extent.Text))
    Set-ServiceConfigurationAcl $config
    $configAcl = Get-Acl -LiteralPath $config
    if (-not $configAcl.AreAccessRulesProtected) { throw 'Service configuration ACL inherited public permissions.' }
    $serviceRules = @($configAcl.Access | Where-Object { $_.IdentityReference.Translate([Security.Principal.SecurityIdentifier]).Value -eq 'S-1-5-19' })
    if ($serviceRules.Count -ne 1 -or ($serviceRules[0].FileSystemRights -band [Security.AccessControl.FileSystemRights]::Read) -ne [Security.AccessControl.FileSystemRights]::Read) {
        throw 'LocalService lacks configuration read access.'
    }
    if (($serviceRules[0].FileSystemRights -band [Security.AccessControl.FileSystemRights]::WriteData) -ne 0) { throw 'LocalService can modify configuration.' }
    & $Binary config validate --config $config
    if ($LASTEXITCODE -ne 0) { throw 'Configuration validation failed after restoring the installer environment.' }
    $deferredPath = Join-Path $testRoot 'deferred.yaml'
    $deferred = 'config_version: 2
telegram:
  enabled: true
  bot_token: "${TRM_INSTALLER_TEST_ABSENT}"
  chat_id: "123"
'
    [IO.File]::WriteAllText($deferredPath, $deferred, [Text.UTF8Encoding]::new($false))
    $report = & $Binary config validate --structural --config $deferredPath
    if ($LASTEXITCODE -ne 0 -or (($report -join "`n") -notmatch 'telegram\.bot_token')) {
        throw 'Structural validation did not report the deferred environment requirement.'
    }
    function Assert-RejectedConfiguration([string[]]$ExtraArguments) {
        $ErrorActionPreference = 'Continue'
        & $Binary config validate @ExtraArguments --config $deferredPath 2>$null | Out-Null
        if ($LASTEXITCODE -ne 2) { throw "Expected configuration rejection exit 2, got $LASTEXITCODE." }
    }
    Assert-RejectedConfiguration @()
    [IO.File]::WriteAllText($deferredPath, $deferred + "poll_interval: soon`n", [Text.UTF8Encoding]::new($false))
    Assert-RejectedConfiguration @('--structural')
    [IO.File]::WriteAllText($deferredPath, $deferred + "unknown_setting: true`n", [Text.UTF8Encoding]::new($false))
    Assert-RejectedConfiguration @('--structural')
    Write-Host 'Installer regressions passed: environment restoration, private service-read ACL, deferred references, strict literal/runtime validation.'
} finally {
    foreach ($name in $saved.Keys) {
        if ($null -eq $saved[$name]) { Remove-Item -LiteralPath ('Env:\' + $name) -ErrorAction SilentlyContinue }
        else { [Environment]::SetEnvironmentVariable($name, $saved[$name], 'Process') }
    }
    $resolved = [IO.Path]::GetFullPath($testRoot)
    $allowed = $tempRoot.TrimEnd('\') + '\TokenResetsMonitor env test '
    if ($resolved.StartsWith($allowed, [StringComparison]::OrdinalIgnoreCase) -and (Test-Path -LiteralPath $resolved)) {
        Remove-Item -LiteralPath $resolved -Recurse -Force
    }
}

# Expected rejection checks leave a nonzero LASTEXITCODE for CI wrappers.
# Report success only after every assertion and cleanup has completed.
exit 0
