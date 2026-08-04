# libntfs Examples

This directory contains example programs demonstrating how to use the libntfs
library.

## Examples

### 1. basic - Volume Information and Directory Listing

Shows how to open an NTFS volume and list the root directory, including deleted
entries marked as `[DEL]`.

```bash
# Linux
./basic /dev/sda1

# Windows (run as Administrator)
basic.exe \\.\C:

# Disk image
./basic disk.img
```

### 2. traverse - Recursive Directory Traversal

Demonstrates recursive directory traversal with statistics. Deleted entries are
shown with `[DELETED]` markers. Deleted directories are listed but not recursed
into.

```bash
./traverse /dev/sda1 /Windows
```

### 3. extract - File Extraction

Extracts a file from an NTFS volume.

```bash
./extract /dev/sda1 /Windows/System32/notepad.exe notepad.exe
```

### 4. windows_drive - Windows-Specific Example

A Windows-specific example with proper drive access handling. Entries recovered
as deleted are marked as `[DEL]`.

```bash
# Run as Administrator
windows_drive.exe C
```

### 5. bench - End-to-End Performance Benchmark

Measures practical parsing speed on a real disk image/volume by combining:

- directory traversal throughput
- sampled file read throughput (optional extraction-like reads)

```bash
# Traverse only (no file-content reads)
./bench -sample-files=0 disk.img /

# Traverse and read up to 2 MiB from 500 sampled files
./bench -sample-files=500 -read-bytes=2097152 disk.img /Windows

# Windows raw drive (run as Administrator)
bench.exe -sample-files=500 -read-bytes=1048576 \\\\.\\C: /
```

### 6. finfo - Structured File Information

Shows structured metadata for a specific file path, including size, start/end
byte offsets, fragmentation, and timestamps.

```bash
./finfo disk.img /Windows/System32/notepad.exe
```

### 7. overwrite - Guarded In-Place File Overwrite

Writes bytes to a file at a logical offset, with a caller-provided end boundary
to prevent accidental out-of-range writes.

On Windows raw volume paths (`\\.\\C:` style), this example attempts
`FSCTL_LOCK_VOLUME` before writing and unlocks on exit.

```bash
./overwrite disk.img /notes.txt 0x10 HELLO 0x40
```

### 8. mftdump - MFT Record Timeline

Lists MFT records as a flat timeline. Accepts either a standalone `$MFT` file,
as extracted by KAPE, FTK Imager, or icat, or a full volume or disk image. The
input type is detected from the first four bytes and can be forced with
`-standalone`.

Unallocated and corrupt records are skipped rather than ending the walk, so a
damaged table still yields everything readable around the damage.

```bash
# Extracted $MFT file, first 100 allocated records
./mftdump -limit 100 ./\$MFT

# Include records no longer marked in use
./mftdump -deleted ./\$MFT

# Whole volume or image
./mftdump disk.img

# JSON Lines for piping; the preamble goes to stderr, records to stdout
./mftdump -json ./\$MFT | jq -r '.name'

# Windows raw drive (run as Administrator)
mftdump.exe \\.\C:
```

## Building Examples

```bash
# Linux/macOS
cd examples/basic
go build

# Windows
cd examples\basic
go build
```

## Important: Windows Drive Access

On Windows, accessing raw drives requires:

1. **Administrator Privileges**: Run PowerShell or Command Prompt as
   Administrator
2. **Correct Path Format**: Use `\\.\C:` format (note the double backslashes)

### Correct Windows Paths

```
\\.\C:              - Drive C:
\\.\D:              - Drive D:
\\.\PhysicalDrive0  - First physical drive
\\.\PhysicalDrive1  - Second physical drive
```

### Common Windows Errors

**Error: "Access is denied"**

- **Solution**: Run as Administrator (right-click → "Run as administrator")

**Error: "The system cannot find the file specified"**

- **Solution**: Use correct path format `\\.\C:` (not just `C:` or `C:\`)

**Error: "invalid MFT record size"**

- **Solution**: Ensure you're opening a valid NTFS volume, not a directory

## Example: Windows PowerShell (Administrator)

```powershell
# Build the example
cd examples\basic
go build

# List available drives
Get-PSDrive -PSProvider FileSystem

# Run the example on drive C:
.\basic.exe \\.\C:

# Or use the Windows-specific example
cd ..\windows_drive
go build
.\windows_drive.exe C
```

## Example: Linux

```bash
# List available devices
lsblk

# Find NTFS partitions
sudo fdisk -l | grep NTFS

# Run the example
sudo ./basic /dev/sda1
```

## Troubleshooting

### "Failed to open volume"

- Linux: Ensure you have permission (use `sudo`)
- Windows: Run as Administrator
- Verify the device path exists

### "Failed to parse NTFS volume"

- Verify the volume is actually NTFS
- Check if the volume is mounted (unmount if necessary on Linux)
- Ensure the volume is not corrupted

### "File not found"

- Windows: Use `\\.\C:` format, not `C:` or `C:\`
- Linux: Use device path like `/dev/sda1`, not mount point

## Testing with Disk Images

You can create test disk images without requiring system access:

### Create a small NTFS image (Linux)

```bash
# Create 100MB image
dd if=/dev/zero of=test.img bs=1M count=100

# Format as NTFS
mkfs.ntfs -F test.img

# Mount and add files
sudo mkdir /mnt/test
sudo mount -o loop test.img /mnt/test
sudo cp some_files /mnt/test/
sudo umount /mnt/test

# Use with libntfs
./basic test.img
```

### Use existing disk images

Many NTFS disk images are available for testing:

- Virtual machine disk images (.vhd, .vmdk)
- Forensic test images
- Backup images

## Performance Tips

1. **Large Directories**: Reading large directories may take time
2. **Concurrent Access**: The library is thread-safe; multiple reads can occur
   simultaneously
3. **Caching**: Frequently accessed MFT entries are cached automatically

## Security Notes

- **Administrator/Root Access**: Required for raw drive access
- **Write Safety**: Write support is intentionally limited and only supports
  in-place overwrite of non-resident, non-sparse, non-compressed, non-encrypted
  streams on writable handles
- **Mounted Volumes**: Be careful accessing mounted volumes; prefer unmounted
  for consistency
