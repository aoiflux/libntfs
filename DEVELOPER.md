# Developer Guide

## Building the Library

```bash
# Clone the repository
git clone https://github.com/yourusername/libntfs
cd libntfs

# Run tests
go test -v ./...

# Run benchmarks
go test -bench=. -benchmem

# Build examples
cd examples/basic
go build
```

## Architecture Overview

### Core Components

1. **Volume** (`volume.go`)
   - Main entry point for NTFS access
   - Manages boot sector parsing
   - Handles MFT initialization
   - Provides thread-safe access via `sync.RWMutex`
   - Maintains MFT entry cache

2. **MFT** (`mft.go`)
   - Parses MFT entries
   - Handles update sequence arrays (fixups)
   - Implements data run resolution
   - Provides MFT entry caching

3. **Attributes** (`attributes.go`)
   - Parses resident and non-resident attributes
   - Handles data run parsing
   - Supports all standard NTFS attribute types
   - Parses complex structures like $INDEX_ROOT

4. **File** (`file.go`)
   - Provides file and directory operations
   - Implements Read/ReadAt interfaces
   - Directory traversal and listing
   - Path-based file access

5. **Binary** (`binary.go`)
   - Low-level binary parsing utilities
   - Little-endian integer reading
   - UTF-16 string conversion
   - Time format conversion

## Thread Safety

All public APIs are thread-safe:

- **Volume-level operations**: Protected by `Volume.mu` (RWMutex)
- **MFT cache**: Protected by `Volume.mftCacheMu` (RWMutex)
- **Buffer pool**: Uses `sync.Pool` for lock-free buffer management
- **Concurrent reads**: Multiple goroutines can read simultaneously

### Example: Concurrent File Reading

```go
volume, _ := libntfs.Open(disk)
defer volume.Close()

var wg sync.WaitGroup
files := []string{"/file1.txt", "/file2.txt", "/file3.txt"}

for _, path := range files {
    wg.Add(1)
    go func(p string) {
        defer wg.Done()
        file, err := volume.OpenPath(p)
        if err != nil {
            return
        }
        data, _ := file.ReadAll()
        // Process data...
    }(path)
}

wg.Wait()
```

## Performance Optimization

### MFT Caching

The library caches up to 1000 MFT entries by default. Adjust via:

```go
// In constants.go
const DefaultMFTCacheSize = 5000  // Increase for better performance
```

### Buffer Pooling

Reusable buffers reduce GC pressure. The library uses `sync.Pool` internally.

### Data Run Reading

Data runs are read in cluster-aligned chunks for efficiency. For large files:

```go
// Read in chunks
buf := make([]byte, 1024*1024)  // 1 MB buffer
for {
    n, err := file.Read(buf)
    if err == io.EOF {
        break
    }
    // Process chunk...
}
```

## Error Handling

The library uses typed errors for better error handling:

```go
file, err := volume.OpenPath("/path/to/file")
if err != nil {
    if errors.Is(err, libntfs.ErrFileNotFound) {
        // Handle file not found
    } else if errors.Is(err, libntfs.ErrNotDirectory) {
        // Handle not a directory
    } else {
        // Other error
    }
}
```

## Testing

### Unit Tests

```bash
go test -v ./...
```

### Benchmarks

```bash
go test -bench=. -benchmem -cpuprofile=cpu.prof
```

### Integration Tests

For integration tests, you'll need a test NTFS volume:

```go
func TestIntegration(t *testing.T) {
    if testing.Short() {
        t.Skip("Skipping integration test")
    }

    disk, err := os.Open("/path/to/test/volume")
    if err != nil {
        t.Skip("Test volume not available")
    }
    defer disk.Close()

    volume, err := libntfs.Open(disk)
    if err != nil {
        t.Fatalf("Failed to open volume: %v", err)
    }
    defer volume.Close()

    // Test operations...
}
```

## Adding New Features

### Adding a New Attribute Type

1. Define constants in `types.go`:
```go
const AttrTypeNewAttribute = 0x999
```

2. Add parsing function in `attributes.go`:
```go
func parseNewAttribute(data []byte) (*NewAttribute, error) {
    // Parse logic...
}
```

3. Update attribute type names in `constants.go`:
```go
var AttributeTypeNames = map[uint32]string{
    // ...
    AttrTypeNewAttribute: "$NEW_ATTRIBUTE",
}
```

### Extending File Operations

Add new methods to the `File` struct in `file.go`:

```go
func (f *File) NewOperation() error {
    if f.volume.IsClosed() {
        return ErrVolumeClosed
    }
    // Implementation...
}
```

## Debugging

Enable verbose output:

```go
import "log"

// Log MFT entry details
entry, _ := volume.GetMFTEntry(5)
log.Printf("MFT Entry: %+v", entry)

// Log attributes
for _, attr := range entry.Attributes {
    log.Printf("Attribute: %s", attr.GetAttributeName())
}
```

## Code Style

- Follow standard Go formatting (`gofmt`)
- Add comments for all exported functions
- Use descriptive variable names
- Keep functions focused and small
- Write tests for new features

## Contributing

1. Fork the repository
2. Create a feature branch
3. Add tests for new functionality
4. Ensure all tests pass
5. Submit a pull request

## License

MIT License - see LICENSE file for details
