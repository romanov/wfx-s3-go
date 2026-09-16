$ErrorActionPreference = 'Stop'

function Add-DirectoryToPath([string] $Directory) {
    if (-not (Test-Path -LiteralPath $Directory -PathType Container)) {
        return
    }
    $pathEntries = @($env:Path -split ';' | Where-Object { $_ -ne '' })
    if ($pathEntries -notcontains $Directory) {
        $env:Path = "$Directory;$env:Path"
    }
}

function Require-Command([string] $Name, [string] $InstallHint) {
    if (-not (Get-Command $Name -ErrorAction SilentlyContinue)) {
        throw "$Name was not found on PATH. $InstallHint"
    }
}

function Invoke-Checked([string] $Command, [string[]] $Arguments) {
    & $Command @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "$Command failed with exit code $LASTEXITCODE."
    }
}

# Support standard Windows installations even when their directories were not
# added to the current PowerShell session's PATH.
foreach ($directory in @(
        'C:\Program Files\Go\bin',
        'C:\Go\bin',
        'C:\msys64\mingw64\bin',
        'C:\msys64\ucrt64\bin',
        'C:\mingw64\bin')) {
    Add-DirectoryToPath $directory
}
Add-DirectoryToPath 'C:\msys64\usr\bin'

Require-Command 'go' 'Install Go 1.24 or newer from https://go.dev/dl/. '
Require-Command 'gcc' 'Install a Windows x64 MinGW-w64 toolchain and add its bin directory to PATH. '
$gccPath = @(
    'C:\msys64\mingw64\bin\gcc.exe',
    'C:\msys64\ucrt64\bin\gcc.exe',
    'C:\mingw64\bin\gcc.exe'
) | Where-Object { Test-Path -LiteralPath $_ -PathType Leaf } | Select-Object -First 1
if (-not $gccPath) {
    $gccPath = (Get-Command 'gcc' -CommandType Application | Select-Object -First 1).Source
}
$env:CC = $gccPath

Write-Host "Using Go: $((Get-Command 'go' -CommandType Application).Source)"
Write-Host "Using GCC: $env:CC"

$goVersion = (& go version)
if ($goVersion -notmatch 'go(?<major>\d+)\.(?<minor>\d+)') {
    throw "Unable to determine the Go version from: $goVersion"
}
if ([int]$Matches.major -eq 1 -and [int]$Matches.minor -lt 24) {
    throw "Go 1.24 or newer is required; found $goVersion"
}

$env:CGO_ENABLED = '1'
$env:GOOS = 'windows'
$env:GOARCH = 'amd64'

New-Item -ItemType Directory -Force -Path '.\dist' | Out-Null
Invoke-Checked 'go' @('mod', 'download')
Invoke-Checked 'go' @('test', './...')
Invoke-Checked 'go' @('build', '-trimpath', '-buildmode=c-shared', '-o', '.\dist\wfxs3.wfx64', '.\cmd\wfxs3')
Copy-Item -Force '.\wfxs3.ini.example' '.\dist\wfxs3.ini.example'

Write-Host 'Built dist\wfxs3.wfx64'
