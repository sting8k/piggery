# Install piggery on Windows from a GitHub release: the binary for this CPU, checked against the
# release's checksums.txt, into $env:PIGGERY_INSTALL_DIR (default %USERPROFILE%\.local\bin). No
# administrator rights.
#
#   irm https://raw.githubusercontent.com/sting8k/piggery/main/install.ps1 | iex
#
# $env:PIGGERY_VERSION = 'vX.Y.Z' installs that release instead of the latest.
#
# For Windows PowerShell 5.1 and PowerShell 7. Plain ASCII on purpose: Windows PowerShell reads a
# file without a BOM as ANSI. Everything is in one script block: piped into iex it leaves no
# variables behind, and a failure (throw) ends the block, not the window.
& {
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue' # the progress bar makes a Windows PowerShell download crawl
    $repo = 'https://github.com/sting8k/piggery/releases'
    $dir = if ($env:PIGGERY_INSTALL_DIR) { $env:PIGGERY_INSTALL_DIR } else { Join-Path $env:USERPROFILE '.local\bin' }

    # A 32-bit PowerShell on a 64-bit Windows says x86; the machine's CPU is then in the second variable.
    $cpu = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
    switch ($cpu) {
        'AMD64' { $arch = 'amd64' }
        'ARM64' { $arch = 'arm64' }
        default { throw "piggery install: unsupported CPU ${cpu}: builds exist for amd64 and arm64" }
    }
    $name = "piggery-windows-$arch.exe"
    $url = if ($env:PIGGERY_VERSION) { "$repo/download/$($env:PIGGERY_VERSION)" } else { "$repo/latest/download" }

    # Windows PowerShell on an older Windows does not offer TLS 1.2 by itself.
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

    $tmp = Join-Path ([IO.Path]::GetTempPath()) ('piggery-install-' + [Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $tmp | Out-Null
    try {
        Write-Host "piggery install: downloading $name from $url"
        $bin = Join-Path $tmp $name
        $sums = Join-Path $tmp 'checksums.txt'
        try { Invoke-WebRequest -UseBasicParsing -Uri "$url/$name" -OutFile $bin }
        catch { throw "piggery install: could not download $url/${name}: $($_.Exception.Message)" }
        try { Invoke-WebRequest -UseBasicParsing -Uri "$url/checksums.txt" -OutFile $sums }
        catch { throw "piggery install: could not download $url/checksums.txt: $($_.Exception.Message)" }

        $want = $null
        foreach ($line in Get-Content -LiteralPath $sums) {
            $f = -split $line
            if ($f.Count -eq 2 -and ($f[1] -eq $name -or $f[1] -eq "*$name")) { $want = $f[0].ToLower() }
        }
        if (-not $want) { throw "piggery install: checksums.txt has no line for $name" }
        $got = (Get-FileHash -LiteralPath $bin -Algorithm SHA256).Hash.ToLower()
        if ($got -ne $want) { throw "piggery install: checksum mismatch for $name (got $got, want $want); nothing installed" }

        New-Item -ItemType Directory -Force -Path $dir | Out-Null
        $target = Join-Path $dir 'piggery.exe'
        $old = Join-Path $dir 'piggery.exe.old'
        $new = Join-Path $dir ".piggery.new.$PID"
        $had = Test-Path -LiteralPath $target
        try {
            Copy-Item -LiteralPath $bin -Destination $new -Force
            # A running piggery (its daemon) can be renamed, not replaced or deleted: the binary
            # there steps aside as piggery.exe.old, the way piggery update does it.
            if ($had) {
                if (Test-Path -LiteralPath $old) { Remove-Item -LiteralPath $old -Force }
                Move-Item -LiteralPath $target -Destination $old
            }
            Move-Item -LiteralPath $new -Destination $target
        } catch {
            $why = $_.Exception.Message
            if ($had -and -not (Test-Path -LiteralPath $target) -and (Test-Path -LiteralPath $old)) {
                Move-Item -LiteralPath $old -Destination $target # back where it was: nothing replaced
            }
            if (Test-Path -LiteralPath $new) { Remove-Item -LiteralPath $new -Force }
            throw "piggery install: could not put the binary in ${dir}: $why"
        }
    } finally {
        Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
    }

    $version = try { & $target --version } catch { $null }
    if (-not $version) { $version = $name }
    Write-Host "piggery install: installed $version in $dir"
    if ($had) {
        Write-Host "piggery install: the binary it replaced is $old (a daemon still running is that one: piggery restart)"
    }
    $onPath = ($env:Path -split ';') | ForEach-Object { $_.TrimEnd('\') }
    if ($onPath -notcontains $dir.TrimEnd('\')) {
        Write-Host "piggery install: $dir is not on your PATH; add it for your user, then open a new terminal:"
        Write-Host "  [Environment]::SetEnvironmentVariable('Path', [Environment]::GetEnvironmentVariable('Path', 'User') + ';$dir', 'User')"
    }
    Write-Host 'Next: on a new machine, piggery setup <harness> (pi, claude, codex, ...; piggery setup alone lists them); upgrading, piggery setup --outdated'
}
