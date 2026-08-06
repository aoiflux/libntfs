<#
.SYNOPSIS
    Builds the real-image test corpus: a small NTFS volume written by Windows.

.DESCRIPTION
    Every other test in this repository builds NTFS structures byte by byte in
    Go, which proves the parsers against bytes libntfs itself wrote. This script
    produces the other half of the picture: a volume Windows formatted and
    populated, carrying one of each shape the library claims to handle.

    It creates a VHD, formats it NTFS, writes the fixtures, copies the raw
    volume out, and compresses it to testdata/ntfs-corpus.img.gz. The copy is
    taken from the volume device rather than the VHD file, so the output starts
    at the NTFS boot sector with no partition table in front of it — which is
    what libntfs.Open expects.

    REQUIRES ADMINISTRATOR. Creating, attaching, and formatting a virtual disk,
    and reading a raw volume device, all need elevation.

    On Linux, `mkntfs` produces an equivalent image without elevation, but it is
    ntfs-3g writing the structures rather than Windows, which is the point of
    this fixture. Prefer this script where you can run it.

.EXAMPLE
    # From an elevated PowerShell prompt, in the repository root:
    .\testdata\make_corpus.ps1

.NOTES
    The image is committed so tests need no tooling to run. Regenerate it only
    when the fixture set changes, and read the size delta before committing:
    the volume is mostly zeros and should compress to well under a megabyte.
#>

[CmdletBinding()]
param(
    [string]$OutputPath = (Join-Path $PSScriptRoot 'ntfs-corpus.img.gz'),
    [int]$SizeMB = 64,
    [string]$VhdPath = (Join-Path $env:TEMP 'libntfs-corpus.vhdx')
)

$ErrorActionPreference = 'Stop'

$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = New-Object Security.Principal.WindowsPrincipal($identity)
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'This script must run from an elevated PowerShell prompt.'
}

$disk = $null

try {
    if (Test-Path $VhdPath) {
        Remove-Item $VhdPath -Force
    }

    Write-Host "Creating $SizeMB MB VHD at $VhdPath"
    New-VHD -Path $VhdPath -SizeBytes ($SizeMB * 1MB) -Fixed | Out-Null

    $disk = Mount-VHD -Path $VhdPath -Passthru | Initialize-Disk -PartitionStyle MBR -Passthru
    $partition = $disk | New-Partition -UseMaximumSize -AssignDriveLetter
    $volume = $partition | Format-Volume -FileSystem NTFS -NewFileSystemLabel 'LIBNTFS' -Confirm:$false

    $drive = "$($partition.DriveLetter):"
    Write-Host "Formatted $drive, writing fixtures"

    # --- Fixtures, one per shape the library distinguishes -------------------

    # A resident file: content small enough to live inside its MFT record.
    Set-Content -Path "$drive\resident.txt" -Value 'inline' -NoNewline

    # A non-resident file large enough to need clusters.
    $filler = [byte[]]::new(256KB)
    [System.Random]::new(1).NextBytes($filler)
    [IO.File]::WriteAllBytes("$drive\plain.bin", $filler)

    # A fragmented file: interleave two growing files so neither stays
    # contiguous, which is how fragmentation actually arises.
    $chunk = [byte[]]::new(64KB)
    [System.Random]::new(2).NextBytes($chunk)
    $a = [IO.File]::Open("$drive\fragmented.bin", 'Create')
    $b = [IO.File]::Open("$drive\spacer.bin", 'Create')
    try {
        for ($i = 0; $i -lt 8; $i++) {
            $a.Write($chunk, 0, $chunk.Length); $a.Flush($true)
            $b.Write($chunk, 0, $chunk.Length); $b.Flush($true)
        }
    } finally {
        $a.Dispose(); $b.Dispose()
    }
    Remove-Item "$drive\spacer.bin" -Force

    # A sparse file: a hole between two written regions.
    & fsutil.exe file createnew "$drive\sparse.bin" 1048576 | Out-Null
    & fsutil.exe sparse setflag "$drive\sparse.bin" | Out-Null
    & fsutil.exe sparse setrange "$drive\sparse.bin" 4096 524288 | Out-Null

    # A compressed file: highly repetitive so NTFS actually compresses it.
    Set-Content -Path "$drive\compressed.txt" -Value ('A' * 200000) -NoNewline
    & compact.exe /C "$drive\compressed.txt" | Out-Null

    # An alternate data stream, the classic hiding place.
    Set-Content -Path "$drive\host.txt" -Value 'visible content' -NoNewline
    Set-Content -Path "$drive\host.txt:Zone.Identifier" -Value "[ZoneTransfer]`r`nZoneId=3" -NoNewline

    # A directory with children, so index parsing has something to walk.
    New-Item -ItemType Directory -Path "$drive\docs" | Out-Null
    1..24 | ForEach-Object { Set-Content -Path "$drive\docs\file$_.txt" -Value "entry $_" -NoNewline }

    # Reparse points: a symlink and a junction.
    & cmd.exe /c mklink "$drive\link.txt" "$drive\host.txt" | Out-Null
    & cmd.exe /c mklink /J "$drive\junction" "$drive\docs" | Out-Null

    # A deleted file, recoverable from index slack.
    Set-Content -Path "$drive\deleted.txt" -Value 'this file is removed' -NoNewline
    Remove-Item "$drive\deleted.txt" -Force

    # Extended attributes are written by WSL rather than by Windows tooling. If
    # a WSL distribution is installed, write two files through it: one carrying
    # LXATTRB (mode and ownership) and one also carrying LXXATTR (POSIX extended
    # attributes).
    #
    # The LXXATTR file matters more than its size suggests. LXXATTR is not a
    # documented structure, so libntfs decodes a layout inferred from what WSL
    # writes. This is the fixture that confirms the inference against a real
    # writer; without it, ParseLinuxExtendedAttributes is only ever tested
    # against bytes libntfs itself produced.
    $wsl = Get-Command wsl.exe -ErrorAction SilentlyContinue
    if ($wsl) {
        $mount = "/mnt/$($partition.DriveLetter.ToString().ToLower())"

        & wsl.exe -e sh -c "echo posix > $mount/wsl-metadata.txt && chmod 754 $mount/wsl-metadata.txt" 2>$null | Out-Null
        $modeOK = ($LASTEXITCODE -eq 0)

        & wsl.exe -e sh -c "echo tagged > $mount/wsl-xattr.txt && setfattr -n user.origin -v corpus $mount/wsl-xattr.txt" 2>$null | Out-Null
        $xattrOK = ($LASTEXITCODE -eq 0)

        if ($modeOK) {
            Write-Host 'Wrote a file through WSL, so the image carries LXATTRB'
        } else {
            Write-Warning 'WSL is present but could not write to the volume; the image has no LXATTRB'
        }

        if ($xattrOK) {
            Write-Host 'Set a POSIX extended attribute, so the image carries LXXATTR'
        } else {
            Write-Warning 'Could not set a POSIX extended attribute (is attr installed? try: sudo apt install attr). The image has no LXXATTR, and ParseLinuxExtendedAttributes stays unvalidated against a real writer.'
        }
    } else {
        Write-Warning 'No WSL found; the image will have no extended attributes'
    }

    # Flush everything to the volume before reading it back.
    [System.GC]::Collect()
    Start-Sleep -Seconds 2

    # --- Copy the raw volume out ---------------------------------------------

    $rawPath = Join-Path $env:TEMP 'libntfs-corpus.img'
    Write-Host "Copying \\.\$drive to $rawPath"

    $source = [IO.File]::Open("\\.\$drive", 'Open', 'Read', 'ReadWrite')
    try {
        $sink = [IO.File]::Create($rawPath)
        try {
            $source.CopyTo($sink, 1MB)
        } finally {
            $sink.Dispose()
        }
    } finally {
        $source.Dispose()
    }

    Write-Host "Compressing to $OutputPath"
    $in = [IO.File]::OpenRead($rawPath)
    try {
        $out = [IO.File]::Create($OutputPath)
        try {
            $gzip = New-Object IO.Compression.GZipStream($out, [IO.Compression.CompressionLevel]::Optimal)
            try {
                $in.CopyTo($gzip, 1MB)
            } finally {
                $gzip.Dispose()
            }
        } finally {
            $out.Dispose()
        }
    } finally {
        $in.Dispose()
    }

    Remove-Item $rawPath -Force

    $size = (Get-Item $OutputPath).Length
    Write-Host "Wrote $OutputPath ($([math]::Round($size / 1KB, 1)) KB)"
    Write-Host 'Now run: go test -run TestCorpus ./...'
}
finally {
    if ($disk) {
        Dismount-VHD -Path $VhdPath -ErrorAction SilentlyContinue
    }
    if (Test-Path $VhdPath) {
        Remove-Item $VhdPath -Force -ErrorAction SilentlyContinue
    }
}
