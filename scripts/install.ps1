# Install a prebuilt runspace binary from the GitHub releases on Windows. It
# downloads the archive for this system, checks it against the release's
# SHA-256 checksums, installs runspace.exe, and adds its directory to the
# user's Path. On Linux and macOS, use install.sh.
#
# Usage: scripts/install.ps1 [-Version <version>] [-InstallDir <dir>] [-NoModifyPath]
#
#   irm https://get.sandbox.ec/runspace.ps1 | iex
#
# The version is a release tag such as v0.1.0 and defaults to
# RUNSPACE_VERSION, or else the latest release. The binary goes into dir,
# which defaults to RUNSPACE_INSTALL_DIR, or else
# %LOCALAPPDATA%\Programs\runspace. Any value in RUNSPACE_NO_MODIFY_PATH
# acts as -NoModifyPath. Piped to iex, the script takes only these
# variables. It needs Windows PowerShell 5.1 or PowerShell 7.

# Everything runs in a script block, so that under iex its parameters and
# preferences stay out of the caller's session; @args passes the script's
# own arguments through when it runs as a file.
& {
    param(
        [string]$Version = $env:RUNSPACE_VERSION,
        [string]$InstallDir = $env:RUNSPACE_INSTALL_DIR,
        [switch]$NoModifyPath
    )

    Set-StrictMode -Version Latest
    $ErrorActionPreference = 'Stop'
    # The progress bar slows Invoke-WebRequest in Windows PowerShell many
    # times over.
    $ProgressPreference = 'SilentlyContinue'

    $repo = 'sandboxec/runspace'

    if ($PSVersionTable.PSVersion.Major -ge 6 -and -not $IsWindows) {
        throw 'this script installs on Windows; on Linux and macOS, use install.sh'
    }
    # A 32-bit PowerShell on 64-bit Windows sees x86 in
    # PROCESSOR_ARCHITECTURE and the real one in PROCESSOR_ARCHITEW6432.
    $machine = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
    $arch = switch ($machine) {
        'AMD64' { 'amd64' }
        'ARM64' { 'arm64' }
        default { throw "unsupported architecture $machine" }
    }
    if (-not $InstallDir) {
        $InstallDir = Join-Path $env:LOCALAPPDATA 'Programs\runspace'
    }
    # An absolute path, so .NET calls resolve it as PowerShell does, and
    # -LiteralPath below, so [ and ] in it are not wildcards.
    $InstallDir = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($InstallDir).TrimEnd('\')

    # Windows PowerShell can default to TLS versions that GitHub refuses.
    [Net.ServicePointManager]::SecurityProtocol =
        [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

    # The release page answers with JSON when asked to, which, unlike the
    # REST API, does not use up the rate limit for anonymous callers.
    if (-not $Version) {
        $release = Invoke-RestMethod -UseBasicParsing -Headers @{ Accept = 'application/json' } `
            -Uri "https://github.com/$repo/releases/latest"
        $Version = $release.tag_name
        if (-not $Version) { throw 'cannot look up the latest release' }
    }
    if (-not $Version.StartsWith('v')) { $Version = "v$Version" }

    # Archive names carry the version without its v.
    $asset = "runspace_$($Version.Substring(1))_windows_$arch.zip"
    $sums = "runspace_$($Version.Substring(1))_checksums.txt"
    $base = "https://github.com/$repo/releases/download/$Version"

    $work = Join-Path ([IO.Path]::GetTempPath()) ([IO.Path]::GetRandomFileName())
    [void][IO.Directory]::CreateDirectory($work)
    try {
        Write-Host "Downloading runspace $Version for windows/$arch"
        try {
            Invoke-WebRequest -UseBasicParsing -Uri "$base/$asset" -OutFile (Join-Path $work $asset)
        } catch {
            throw "cannot download $base/$asset; is $Version a release? $_"
        }
        Invoke-WebRequest -UseBasicParsing -Uri "$base/$sums" -OutFile (Join-Path $work $sums)

        $want = @(Get-Content -LiteralPath (Join-Path $work $sums) | ForEach-Object {
                $fields = -split $_
                if ($fields.Count -eq 2 -and $fields[1] -eq $asset) { $fields[0] }
            })
        if ($want.Count -ne 1) { throw "$sums has no checksum for $asset" }
        $got = (Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $work $asset)).Hash
        if ($got -ne $want[0]) {
            throw "checksum mismatch for ${asset}: got $($got.ToLower()), want $($want[0])"
        }

        $unpacked = Join-Path $work 'unpacked'
        Expand-Archive -LiteralPath (Join-Path $work $asset) -DestinationPath $unpacked

        [void][IO.Directory]::CreateDirectory($InstallDir)
        $exe = Join-Path $InstallDir 'runspace.exe'
        # A running runspace.exe, such as an MCP server, can be renamed but
        # not overwritten or deleted. Move it aside under a unique name, and
        # remove the ones that earlier installs left once they are free.
        Get-ChildItem -LiteralPath $InstallDir -Filter 'runspace.exe.*.old' |
            Remove-Item -Force -ErrorAction SilentlyContinue
        $old = $null
        if (Test-Path -LiteralPath $exe) {
            $old = "$exe.$([IO.Path]::GetRandomFileName()).old"
            Move-Item -LiteralPath $exe -Destination $old
        }
        # Put the old binary back unless the new one runs.
        try {
            Move-Item -LiteralPath (Join-Path $unpacked 'runspace.exe') -Destination $exe
            $installed = & $exe -V
            if ($LASTEXITCODE -ne 0) { throw "$exe -V exited with status $LASTEXITCODE" }
        } catch {
            if ($old) { Move-Item -Force -LiteralPath $old -Destination $exe }
            throw
        }
        if ($old) { Remove-Item -Force -ErrorAction SilentlyContinue -LiteralPath $old }
    } finally {
        Remove-Item -Recurse -Force -ErrorAction SilentlyContinue -LiteralPath $work
    }
    Write-Host "Installed $installed to $exe"

    if ($NoModifyPath -or $env:RUNSPACE_NO_MODIFY_PATH) { return }
    $key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey('Environment')
    try {
        # Read the raw value, so entries such as %USERPROFILE%\bin stay
        # unexpanded when it is written back.
        $raw = [string]$key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
        $entries = @($raw -split ';' | Where-Object { $_ })
        $present = @($entries | Where-Object {
                [Environment]::ExpandEnvironmentVariables($_).TrimEnd('\') -eq $InstallDir
            })
        if ($present.Count -eq 0) {
            $key.SetValue('Path', (($entries + $InstallDir) -join ';'), [Microsoft.Win32.RegistryValueKind]::ExpandString)
            # Setting a user variable through .NET broadcasts the change,
            # so that new terminals see the new Path; the registry write
            # alone does not.
            [Environment]::SetEnvironmentVariable('RUNSPACE_INSTALL', '1', 'User')
            [Environment]::SetEnvironmentVariable('RUNSPACE_INSTALL', $null, 'User')
            Write-Host "Added $InstallDir to your user Path; new terminals will find runspace."
        }
    } finally {
        $key.Close()
    }
    if (-not (@($env:Path -split ';' | ForEach-Object { $_.TrimEnd('\') }) -contains $InstallDir)) {
        $env:Path = "$env:Path;$InstallDir"
    }
} @args
