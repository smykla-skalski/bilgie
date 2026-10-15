# Guarded by BILGIE_OS_E2E because it registers a real scheduled task and
# wipes the state dir of the invoking user; only run it in a throwaway session.
#
# usage: $env:BILGIE_OS_E2E = '1'; agent-windows.ps1 <bilgie.exe>
param([Parameter(Mandatory = $true)][string]$Bin)

$ErrorActionPreference = 'Stop'

if ($env:BILGIE_OS_E2E -ne '1') {
    Write-Error 'refusing to touch the user''s scheduled tasks; set BILGIE_OS_E2E=1 in a throwaway session'
    exit 2
}

$exe = (Resolve-Path $Bin).Path
$stateDir = Join-Path $HOME '.local\state\bilgie'
$tickFile = Join-Path $stateDir 'tick.json'
$taskName = 'bilgie'
$waitSeconds = if ($env:BILGIE_TICK_WAIT) { [int]$env:BILGIE_TICK_WAIT } else { 120 }

function Fail([string]$Message) {
    Write-Host "FAIL: $Message"
    exit 1
}

function Invoke-Bilgie([string[]]$Arguments) {
    & $exe @Arguments
    if ($LASTEXITCODE -ne 0) { Fail "bilgie $($Arguments -join ' ') exited $LASTEXITCODE" }
}

Remove-Item -Recurse -Force $stateDir -ErrorAction SilentlyContinue

Write-Host '::group::install'
Invoke-Bilgie @('install-agent')
Write-Host '::endgroup::'

Write-Host '::group::verify the registered task'
$task = Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
if (-not $task) { Fail 'task not registered' }
Write-Host "state: $($task.State)"
if ($task.State -eq 'Disabled') { Fail 'task is disabled' }
$action = $task.Actions | Select-Object -First 1
Write-Host "action: $($action.Execute) $($action.Arguments)"
if ($action.Execute -ne 'cmd.exe') { Fail "unexpected action $($action.Execute)" }
if ($action.Arguments -notlike "*$exe*tick*") { Fail 'action does not run tick of this binary' }
$repetition = $task.Triggers[0].Repetition
Write-Host "repeat every: $($repetition.Interval)"
if ($repetition.Interval -ne 'PT2M') { Fail "unexpected interval $($repetition.Interval)" }
if (Test-Path $tickFile) { Fail 'tick.json exists before the task ran' }
schtasks.exe /Query /TN $taskName /FO LIST /V
Write-Host '::endgroup::'

Write-Host '::group::run the task once'
Start-ScheduledTask -TaskName $taskName
$waited = 0
while (-not (Test-Path $tickFile)) {
    if ($waited -ge $waitSeconds) {
        Get-ScheduledTaskInfo -TaskName $taskName | Format-List *
        Fail "no tick within ${waitSeconds}s"
    }
    Start-Sleep -Seconds 2
    $waited += 2
}
Write-Host "tick fired after ~${waited}s"
$tick = Get-Content -Raw $tickFile | ConvertFrom-Json
Write-Host (Get-Content -Raw $tickFile)
if (-not ($tick.time -and $tick.tier -and $tick.action)) { Fail 'tick.json lacks time, tier or action' }
# 267009 is "running"; the result only settles once the process has exited.
for ($i = 0; $i -lt 30; $i++) {
    $info = Get-ScheduledTaskInfo -TaskName $taskName
    if ($info.LastTaskResult -ne 267009) { break }
    Start-Sleep -Seconds 1
}
Write-Host "last task result: $($info.LastTaskResult)"
if ($info.LastTaskResult -ne 0) { Fail "task result $($info.LastTaskResult)" }
Write-Host '::endgroup::'

Write-Host '::group::uninstall'
Invoke-Bilgie @('uninstall-agent')
if (Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue) { Fail 'task still registered' }
if (Test-Path $tickFile) { Fail 'tick.json left behind' }
if (Test-Path (Join-Path $stateDir "$taskName-task.xml")) { Fail 'task definition left behind' }
Write-Host '::endgroup::'

Write-Host 'OK: task install, tick and uninstall'
