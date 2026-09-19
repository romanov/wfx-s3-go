//go:build windows

package abismoke

import (
	"os"
	"os/exec"
	"testing"
)

const unloadProbeDLL = "WFXS3_UNLOAD_DLL"

const unloadProbeScript = `
Add-Type @"
using System;
using System.Runtime.InteropServices;
public static class WFXS3UnloadProbe {
  [DllImport("kernel32.dll", CharSet=CharSet.Unicode, SetLastError=true)]
  public static extern IntPtr LoadLibrary(string path);
  [DllImport("kernel32.dll", SetLastError=true)]
  public static extern IntPtr GetProcAddress(IntPtr h, string name);
  [DllImport("kernel32.dll", SetLastError=true)]
  [return: MarshalAs(UnmanagedType.Bool)]
  public static extern bool FreeLibrary(IntPtr h);
  [UnmanagedFunctionPointer(CallingConvention.Winapi)]
  public delegate void Root(IntPtr buffer, int maxLen);
}
"@

$path = [Environment]::GetEnvironmentVariable("WFXS3_UNLOAD_DLL")
$handle = [WFXS3UnloadProbe]::LoadLibrary($path)
if ($handle -eq [IntPtr]::Zero) {
  throw "LoadLibrary failed: $([Runtime.InteropServices.Marshal]::GetLastWin32Error())"
}

$address = [WFXS3UnloadProbe]::GetProcAddress($handle, "FsGetDefRootName")
if ($address -eq [IntPtr]::Zero) {
  throw "GetProcAddress failed: $([Runtime.InteropServices.Marshal]::GetLastWin32Error())"
}

$buffer = [Runtime.InteropServices.Marshal]::AllocHGlobal(260)
try {
  $root = [Runtime.InteropServices.Marshal]::GetDelegateForFunctionPointer(
    $address, [WFXS3UnloadProbe+Root])
  $root.Invoke($buffer, 260)
  $value = [Runtime.InteropServices.Marshal]::PtrToStringAnsi($buffer)
  if ($value -ne "S3 API Endpoints") {
    throw "unexpected root name: $value"
  }
} finally {
  [Runtime.InteropServices.Marshal]::FreeHGlobal($buffer)
}

if (-not [WFXS3UnloadProbe]::FreeLibrary($handle)) {
  throw "FreeLibrary failed: $([Runtime.InteropServices.Marshal]::GetLastWin32Error())"
}
`

func TestWFXUnloadAfterRootName(t *testing.T) {
	filename := os.Getenv("WFXS3_DLL")
	if filename == "" {
		t.Skip("set WFXS3_DLL to the built wfxs3.wfx64 path")
	}
	powershell, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("powershell.exe is required for the native DLL unload probe")
	}

	cmd := exec.Command(powershell, "-NoProfile", "-NonInteractive", "-Command", unloadProbeScript)
	cmd.Env = append(os.Environ(), unloadProbeDLL+"="+filename)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("DLL unload probe failed: %v\n%s", err, output)
	}
}
