# KeibiDrop FUSE Testing

Standalone test scripts for verifying FUSE filesystem correctness. These tests mount a local FUSE filesystem (no peer connection needed) and run workloads against it.

## Prerequisites

- Go 1.24+
- FUSE libraries installed:
  - **macOS**: [macFUSE](https://osxfuse.github.io/)
  - **Linux**: `sudo apt install libfuse-dev fuse` and `user_allow_other` in `/etc/fuse.conf`
- PostgreSQL 16+ (for database tests): `brew install postgresql@16` or `sudo apt install postgresql-16`
- pjdfstest (for POSIX compliance): clone from https://github.com/pjd/pjdfstest

## Quick start

```bash
# Build the standalone FUSE mount tool
make build-mount-fuse

# Run all tests
make test-fuse

# Individual tests
make test-fstest          # POSIX compliance (requires pjdfstest)
make test-postgresql      # PostgreSQL initdb + CRUD
make test-git-clone       # git clone integrity
```

## What each test does

**test-fstest**: Mounts FUSE, runs pjdfstest suite (excluding symlink/hardlink), reports pass rate. Requires root for chown tests.

**test-postgresql**: Mounts FUSE, runs `initdb`, starts PostgreSQL, creates a table, inserts 1000 rows, queries, shuts down. Verifies file count matches native initdb.

**test-git-clone**: Mounts FUSE, clones a small repo into the mount, verifies `git log` and `git status` work correctly. Tests the file rename/write patterns that git uses.
