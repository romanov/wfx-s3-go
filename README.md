# WFX S3 for Total Commander

<img width="650" height="325" alt="653090478-05c930c4-32f1-43f9-b83c-0093c59455b8(1)" src="https://github.com/user-attachments/assets/aedca9ba-1624-4fa1-bb86-946f9a9eff8c" />


This is an x64-only Total Commander file-system plugin written in Go. It
exposes S3-compatible buckets as virtual folders and supports browsing,
uploading, downloading, and deleting objects.

The repository includes the Total Commander WFX SDK under
`WFX-SDK-master`. The plugin is built as a Unicode Windows x64 c-shared DLL
with the required `.wfx64` extension.

## Requirements

- Total Commander x64.
- Go 1.24 or newer.
- A Windows x64 cgo compiler, such as MinGW-w64 GCC.

The current AWS SDK for Go v2 also requires Go 1.24 or newer.

## Build

Open PowerShell in the repository root and run:

```powershell
.\build.ps1
```

The plugin is written to `dist\wfxs3.wfx64`.

## Configure

Copy `wfxs3.ini.example` to the directory containing Total Commander's
`wincmd.ini`, rename it to `wfxs3.ini`, and add one INI section per profile:

```ini
[my-server]
endpoint=https://s3.example.com
region=us-east-1
bucket=my-bucket
prefix=documents
access_key=ACCESS_KEY
secret_key=SECRET_KEY
session_token=
path_style=true
```

The profile name becomes a directory under the plugin root. For example,
`\\S3 (Go)\\my-server\\documents` is represented internally as the configured
bucket and prefix.

`endpoint`, `bucket`, `access_key`, and `secret_key` are required. `region`,
`prefix`, and `session_token` are optional. If `region` is omitted or blank,
the plugin uses `us-east-1` for SigV4 signing. Set `region=auto` or another
provider-specific value when the service requires a different signing scope.
`path_style` defaults to `true`, which is usually the most compatible choice
for custom S3 endpoints.

S3-compatible storage has no native directory objects. The plugin exposes
common prefixes and directory-marker objects (keys ending in `/`) as folders,
including folders that contain no files. A completely empty bucket has no
directory entries to return until a marker or another object is created.

The MVP stores credentials in plaintext because Total Commander's secure
password callback was deliberately not enabled. Restrict access to
`wfxs3.ini` and use a later version with encrypted credential storage for
shared or untrusted machines.

## Install

1. Build the plugin.
2. In Total Commander, open Configuration → Options → Plugins → FS Plugins.
3. Click Add and select `dist\wfxs3.wfx64`.
4. Open Network Neighborhood and enter the `S3 (Go)` plugin root.

## Current limitations

- x64 only; there is no 32-bit build.
- Foreground transfers only.
- No resume or multipart upload support.
- Virtual folders are browse-only; mkdir and remove-directory are not exposed.
- Rename, remote copy, attributes, and timestamps are not exposed.
- S3 object keys containing backslashes cannot be addressed through the WFX
  path syntax.
