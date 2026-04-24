# libntfs Examples

This directory contains example programs demonstrating how to use the libntfs library.

## Examples

### 1. basic - Volume Information and Directory Listing

Shows how to open an NTFS volume and list the root directory.

```bash
# Linux
./basic /dev/sda1

# Windows (run as Administrator)
basic.exe \\.\C:

# Disk image
./basic disk.img
```

### 2. traverse - Recursive Directory Traversal

Demonstrates recursive directory traversal with statistics.

```bash
./traverse /dev/sda1 /Windows
```

### 3. extract - File Extraction

Extracts a file from an NTFS volume.

```bash
./extract /dev/sda1 /Windows/System32/notepad.exe notepad.exe
```

### 4. windows_drive - Windows-Specific Example

A Windows-specific example with proper drive access handling.

```bash
# Run as Administrator
windows_drive.exe C
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

1. **Administrator Privileges**: Run PowerShell or Command Prompt as Administrator
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
2. **Concurrent Access**: The library is thread-safe; multiple reads can occur simultaneously
3. **Caching**: Frequently accessed MFT entries are cached automatically

## Security Notes

- **Administrator/Root Access**: Required for raw drive access
- **Read-Only**: This library only reads data; it cannot modify NTFS volumes
- **Mounted Volumes**: Be careful accessing mounted volumes; prefer unmounted for consistency
