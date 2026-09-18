//go:build windows

package abismoke

import (
	"os"
	"os/exec"
	"testing"
)

const executeProbeDLL = "WFXS3_EXECUTE_DLL"

// executeProbeScript checks the FsExecuteFileW verbs that return without
// user interface. Exporting FsExecuteFileW makes Total Commander call it for
// Enter on a file, so "open" must keep returning FS_EXEC_YOURSELF (-1) for
// Total Commander to download and open the object. "properties" shows the
// modal debug dialog and is therefore not probed.
const executeProbeScript = `
$ErrorActionPreference = 'Stop'
Add-Type @"
using System;
using System.Runtime.InteropServices;
public static class WFXS3ExecuteProbe {
  [DllImport("kernel32.dll", CharSet=CharSet.Unicode, SetLastError=true)]
  public static extern IntPtr LoadLibrary(string path);
  [DllImport("kernel32.dll", SetLastError=true)]
  public static extern IntPtr GetProcAddress(IntPtr h, string name);
  [UnmanagedFunctionPointer(CallingConvention.Winapi)]
  public delegate int InitW(int pluginNr, IntPtr progress, IntPtr log, IntPtr request);
  [UnmanagedFunctionPointer(CallingConvention.Winapi, CharSet=CharSet.Unicode)]
  public delegate int ExecuteFileW(IntPtr mainWin, string remoteName, string verb);
}
"@

$marshal = [Runtime.InteropServices.Marshal]
$handle = [WFXS3ExecuteProbe]::LoadLibrary([Environment]::GetEnvironmentVariable("WFXS3_EXECUTE_DLL"))
if ($handle -eq [IntPtr]::Zero) {
  throw "LoadLibrary failed: $($marshal::GetLastWin32Error())"
}
function Get-Export([string] $name, [Type] $type) {
  $address = [WFXS3ExecuteProbe]::GetProcAddress($handle, $name)
  if ($address -eq [IntPtr]::Zero) {
    throw "missing export $name"
  }
  return $marshal::GetDelegateForFunctionPointer($address, $type)
}
$initW = Get-Export 'FsInitW' ([WFXS3ExecuteProbe+InitW])
$executeFileW = Get-Export 'FsExecuteFileW' ([WFXS3ExecuteProbe+ExecuteFileW])

[void]$initW.Invoke(1, [IntPtr]::Zero, [IntPtr]::Zero, [IntPtr]::Zero)
$cases = @(
  @{ Verb = 'open'; Want = -1 },
  @{ Verb = 'Open'; Want = -1 },
  @{ Verb = 'chmod 755'; Want = 1 },
  @{ Verb = 'quote debug'; Want = 1 }
)
foreach ($case in $cases) {
  $got = $executeFileW.Invoke([IntPtr]::Zero, '\probe\file.txt', $case.Verb)
  if ($got -ne $case.Want) {
    throw "FsExecuteFileW verb '$($case.Verb)' returned $got, want $($case.Want)"
  }
}
`

func TestWFXExecuteFileVerbs(t *testing.T) {
	filename := os.Getenv("WFXS3_DLL")
	if filename == "" {
		t.Skip("set WFXS3_DLL to the built wfxs3.wfx64 path")
	}
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("powershell.exe is required for the native execute probe")
	}

	cmd := exec.Command(powershell, "-NoProfile", "-NonInteractive", "-Command", executeProbeScript)
	cmd.Env = append(os.Environ(), executeProbeDLL+"="+filename)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("DLL execute probe failed: %v\n%s", err, output)
	}
}
