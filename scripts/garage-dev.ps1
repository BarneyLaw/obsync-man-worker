<#
.SYNOPSIS
PowerShell front end for scripts/garage-dev.sh.

.DESCRIPTION
Runs the bash script inside Git Bash, in this session, and waits for it. For
up and env it also sets GARAGE_* in this PowerShell session, so the worker and
obsync pick Garage up straight away.

Invoking scripts/garage-dev.sh directly from PowerShell does not do this:
Windows hands the .sh file to its associated program in a separate window and
returns at once, before Garage is running.

.EXAMPLE
./scripts/garage-dev.ps1 up      # start (idempotent) and set GARAGE_* here
./scripts/garage-dev.ps1 env     # set GARAGE_* for an instance already running
./scripts/garage-dev.ps1 down    # stop and delete it, data included
#>
param(
    [Parameter(Position = 0)]
    [ValidateSet('up', 'env', 'down')]
    [string] $Command = 'up'
)

$ErrorActionPreference = 'Stop'

$bash = @(
    (Join-Path $env:ProgramFiles 'Git\bin\bash.exe'),
    (Join-Path ${env:ProgramFiles(x86)} 'Git\bin\bash.exe')
) | Where-Object { $_ -and (Test-Path $_) } | Select-Object -First 1
if (-not $bash) {
    # Git installed elsewhere: bash.exe sits beside git's cmd directory.
    $git = Get-Command git -ErrorAction SilentlyContinue
    if ($git) {
        $candidate = Join-Path (Split-Path (Split-Path $git.Source)) 'bin\bash.exe'
        if (Test-Path $candidate) { $bash = $candidate }
    }
}
if (-not $bash) { throw 'Git Bash not found. Install Git for Windows, or run scripts/garage-dev.sh from Git Bash.' }

$script = Join-Path $PSScriptRoot 'garage-dev.sh'

if ($Command -eq 'down') {
    & $bash $script down
    exit $LASTEXITCODE
}

$lines = & $bash $script $Command --plain
if ($LASTEXITCODE -ne 0) { throw "garage-dev.sh $Command failed (exit $LASTEXITCODE); see the messages above." }

$set = 0
foreach ($line in $lines) {
    if ($line -match '^(GARAGE_\w+)=(.*)$') {
        Set-Item "env:$($Matches[1])" $Matches[2]
        $set++
    }
}
if ($set -eq 0) { throw 'garage-dev.sh printed no GARAGE_* variables.' }
Write-Host "GARAGE_* set for this session: $env:GARAGE_ENDPOINT, bucket $env:GARAGE_BUCKET"
