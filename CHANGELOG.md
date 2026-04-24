# Changelog

All notable changes to the libntfs project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - 2025-10-18

### Added
- Initial release of libntfs
- Core NTFS parsing functionality
  - Boot sector parsing with full validation
  - MFT entry reading and caching
  - Resident and non-resident attribute parsing
  - Data run parsing and resolution
  - Update sequence array (fixup) validation
- File system operations
  - Volume opening and initialization
  - File reading via path or MFT entry number
  - Directory listing and traversal
  - Metadata access (standard information, file names)
- Thread-safe API design
  - RWMutex for concurrent access
  - Thread-safe MFT caching
  - Buffer pooling for performance
- Comprehensive error handling
  - Typed error structures
  - Error wrapping with context
  - Detailed error messages
- Data structure support
  - $STANDARD_INFORMATION
  - $FILE_NAME (all namespaces)
  - $DATA (resident and non-resident)
  - $INDEX_ROOT
  - $INDEX_ALLOCATION
  - $ATTRIBUTE_LIST
  - $VOLUME_INFORMATION
  - $OBJECT_ID
  - $BITMAP
  - $REPARSE_POINT (detection)
- Utility functions
  - NTFS time conversion
  - UTF-16 string handling
  - Little-endian binary parsing
  - Variable-length integer parsing
- Documentation
  - Comprehensive README
  - Developer guide (DEVELOPER.md)
  - Project summary (PROJECT_SUMMARY.md)
  - Code examples (3 working examples)
  - API documentation via godoc
- Testing
  - Unit tests for core functionality
  - Benchmark tests
  - Test coverage for critical paths
- Examples
  - Basic volume access and directory listing
  - Recursive directory traversal
  - File extraction

### Features by Component

#### volume.go
- Thread-safe volume opening
- Boot sector validation
- MFT initialization
- Cluster/sector calculations
- Buffer pooling

#### mft.go
- MFT entry parsing
- Update sequence application
- Entry caching (1000 entries default)
- Data run offset calculation
- Attribute enumeration

#### attributes.go
- Resident attribute parsing
- Non-resident attribute parsing
- Data run parsing with sparse support
- Attribute-specific parsers:
  - StandardInformation
  - FileName
  - IndexRoot
  - VolumeInformation
  - ObjectID

#### file.go
- File opening by path or entry number
- Read/ReadAt/ReadAll operations
- Directory listing (ReadDir)
- Metadata retrieval
- Index allocation parsing

#### binary.go
- Little-endian integer reading (8/16/32/64-bit)
- File reference parsing (48-bit)
- NTFS time conversion
- UTF-16 to UTF-8 string conversion
- Alignment utilities

#### errors.go
- VolumeError
- MFTError
- AttributeError
- ParseError
- IOError
- Common error constants

#### constants.go
- NTFS magic values
- Attribute type constants
- File attribute flags
- MFT entry constants
- Boot sector constants
- Namespace constants
- Human-readable name mappings

### Known Limitations
- Compressed data not supported (detection only)
- Encrypted data not supported (detection only)
- USN Journal not fully implemented
- Security descriptors not parsed
- Alternate data streams not enumerated
- Reparse points detected but not fully parsed

### Technical Details
- Pure Go implementation (no CGO)
- Zero external dependencies
- Go 1.21+ required
- Cross-platform support
- Thread-safe by design
- Production-ready error handling

### Documentation Files
- README.md - User guide and quick start
- LICENSE - MIT License
- DEVELOPER.md - Development guidelines
- PROJECT_SUMMARY.md - Technical overview
- CHANGELOG.md - This file
- .gitignore - Git ignore rules

### Examples Provided
1. **basic** - Volume info and root directory listing
2. **traverse** - Recursive directory traversal with statistics
3. **extract** - Extract a file from NTFS volume

### Testing Coverage
- Binary parsing utilities
- Time conversion functions
- Data run parsing
- Attribute type lookups
- Error wrapping
- Buffer boundary checks
- UTF-16 string conversion
- File reference parsing
- Variable integer parsing

---

## [Unreleased]

### Planned Features
- [ ] Compression support (LZNT1)
- [ ] Encryption support (EFS)
- [ ] USN Journal full support
- [ ] Security descriptor parsing
- [ ] Alternate data stream enumeration
- [ ] Enhanced caching strategies
- [ ] Memory-mapped I/O option
- [ ] Write support (future major version)

### Planned Improvements
- [ ] More comprehensive test coverage
- [ ] Integration tests with test volumes
- [ ] Performance profiling and optimization
- [ ] Additional examples
- [ ] Extended documentation
- [ ] API stability guarantees for v1.0

---

## Version History

- **0.1.0** (2025-10-18) - Initial release

## Links

- [GitHub Repository](https://github.com/yourusername/libntfs)
- [Issue Tracker](https://github.com/yourusername/libntfs/issues)
- [Documentation](https://pkg.go.dev/github.com/yourusername/libntfs)
