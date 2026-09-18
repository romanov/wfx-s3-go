//go:build windows

package abismoke

import (
	"os"
	"os/exec"
	"testing"
)

const findProbeDLL = "WFXS3_FIND_DLL"

// findProbeScript drives the plugin the way Total Commander does: FsInitW with
// no callbacks, FsSetDefaultParams with an INI path encoded in the ANSI code
// page, then a listing of the plugin root. The settings directory contains a
// non-ASCII character that the current ANSI code page can represent.
const findProbeScript = `
$ErrorActionPreference = 'Stop'
Add-Type @"
using System;
using System.Runtime.InteropServices;
public static class WFXS3FindProbe {
  [DllImport("kernel32.dll", CharSet=CharSet.Unicode, SetLastError=true)]
  public static extern IntPtr LoadLibrary(string path);
  [DllImport("kernel32.dll", SetLastError=true)]
  public static extern IntPtr GetProcAddress(IntPtr h, string name);
  [DllImport("kernel32.dll")]
  public static extern uint GetACP();
  [UnmanagedFunctionPointer(CallingConvention.Winapi)]
  public delegate int InitW(int pluginNr, IntPtr progress, IntPtr log, IntPtr request);
  [UnmanagedFunctionPointer(CallingConvention.Winapi)]
  public delegate void SetDefaultParams(IntPtr parameters);
  [UnmanagedFunctionPointer(CallingConvention.Winapi, CharSet=CharSet.Unicode)]
  public delegate IntPtr FindFirstW(string path, IntPtr findData);
  [UnmanagedFunctionPointer(CallingConvention.Winapi)]
  public delegate int FindNextW(IntPtr handle, IntPtr findData);
  [UnmanagedFunctionPointer(CallingConvention.Winapi)]
  public delegate int FindClose(IntPtr handle);
}
"@

$marshal = [Runtime.InteropServices.Marshal]
$acp = [WFXS3FindProbe]::GetACP()
$encoding = [Text.Encoding]::GetEncoding([int]$acp)
$marker = $null
foreach ($code in @(0x00E9, 0x0416, 0x4E2D, 0x00FC, 0x0142)) {
  $candidate = [string][char]$code
  if ($encoding.GetString($encoding.GetBytes($candidate)) -eq $candidate) {
    $marker = $candidate
    break
  }
}
if (-not $marker) {
  throw "no non-ASCII test character for ANSI code page $acp"
}

$root = Join-Path ([IO.Path]::GetTempPath()) ('wfxs3-abi-' + $marker + '-' + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $root | Out-Null
try {
  [IO.File]::WriteAllLines((Join-Path $root 'wfxs3.ini'), [string[]]@(
    '[probe]',
    'endpoint=https://s3.example.test',
    'bucket=bucket',
    'access_key=access',
    'secret_key=secret'))

  $handle = [WFXS3FindProbe]::LoadLibrary([Environment]::GetEnvironmentVariable("WFXS3_FIND_DLL"))
  if ($handle -eq [IntPtr]::Zero) {
    throw "LoadLibrary failed: $($marshal::GetLastWin32Error())"
  }
  function Get-Export([string] $name, [Type] $type) {
    $address = [WFXS3FindProbe]::GetProcAddress($handle, $name)
    if ($address -eq [IntPtr]::Zero) {
      throw "missing export $name"
    }
    return $marshal::GetDelegateForFunctionPointer($address, $type)
  }
  $initW = Get-Export 'FsInitW' ([WFXS3FindProbe+InitW])
  $setDefaultParams = Get-Export 'FsSetDefaultParams' ([WFXS3FindProbe+SetDefaultParams])
  $findFirstW = Get-Export 'FsFindFirstW' ([WFXS3FindProbe+FindFirstW])
  $findNextW = Get-Export 'FsFindNextW' ([WFXS3FindProbe+FindNextW])
  $findClose = Get-Export 'FsFindClose' ([WFXS3FindProbe+FindClose])

  # FsDefaultParamStruct: int size; DWORD low; DWORD hi; char DefaultIniName[MAX_PATH].
  $iniName = $encoding.GetBytes((Join-Path $root 'wincmd.ini'))
  if ($iniName.Length -ge 260) {
    throw "temporary path is too long for DefaultIniName"
  }
  $parameters = $marshal::AllocHGlobal(272)
  $findData = $marshal::AllocHGlobal(592)
  try {
    $marshal::Copy((New-Object byte[] 272), 0, $parameters, 272)
    $marshal::WriteInt32($parameters, 0, 272)
    $marshal::WriteInt32($parameters, 4, 30)
    $marshal::WriteInt32($parameters, 8, 2)
    $marshal::Copy($iniName, 0, [IntPtr]::Add($parameters, 12), $iniName.Length)

    [void]$initW.Invoke(1, [IntPtr]::Zero, [IntPtr]::Zero, [IntPtr]::Zero)
    $setDefaultParams.Invoke($parameters)
    $find = $findFirstW.Invoke('\', $findData)
    if ($find.ToInt64() -eq -1) {
      throw "FsFindFirstW could not list the plugin root; the INI in $root was not found"
    }
    try {
      $name = $marshal::PtrToStringUni([IntPtr]::Add($findData, 44))
      if ($name -ne 'probe') {
        throw "unexpected root entry: $name"
      }
      if (($marshal::ReadInt32($findData, 0) -band 0x10) -eq 0) {
        throw "profile entry is not a directory"
      }
      $timeLow = $marshal::ReadInt32($findData, 20)
      $timeHigh = $marshal::ReadInt32($findData, 24)
      if ($timeLow -ne -2 -or $timeHigh -ne -1) {
        throw "profile entry has time stamp $timeLow/$timeHigh instead of the no-time value"
      }
      if ($findNextW.Invoke($find, $findData) -ne 0) {
        throw "expected exactly one root entry"
      }
    } finally {
      [void]$findClose.Invoke($find)
    }
  } finally {
    $marshal::FreeHGlobal($parameters)
    $marshal::FreeHGlobal($findData)
  }
} finally {
  Remove-Item -LiteralPath $root -Recurse -Force -ErrorAction SilentlyContinue
}
`

func TestWFXFindFirstWithANSIIniPath(t *testing.T) {
	filename := os.Getenv("WFXS3_DLL")
	if filename == "" {
		t.Skip("set WFXS3_DLL to the built wfxs3.wfx64 path")
	}
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("powershell.exe is required for the native find probe")
	}

	cmd := exec.Command(powershell, "-NoProfile", "-NonInteractive", "-Command", findProbeScript)
	cmd.Env = append(os.Environ(), findProbeDLL+"="+filename)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("DLL find probe failed: %v\n%s", err, output)
	}
}
