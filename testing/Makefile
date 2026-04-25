MOUNT_FUSE_SRC ?= mount-fuse.go
MOUNT_FUSE_BIN ?= ./mount-fuse
FSTEST_DIR     ?= /root/fstest
PG_INITDB      ?= $(shell which initdb 2>/dev/null || find /usr/lib/postgresql /usr/local -name initdb 2>/dev/null | head -1)
PG_CTL         ?= $(shell which pg_ctl 2>/dev/null || find /usr/lib/postgresql /usr/local -name pg_ctl 2>/dev/null | head -1)
PSQL           ?= $(shell which psql)

.PHONY: build-mount-fuse test-fuse test-fstest test-postgresql test-git-clone bench clean

build-mount-fuse:
	@echo "Building mount-fuse..."
	go build -o $(MOUNT_FUSE_BIN) $(MOUNT_FUSE_SRC)
	@echo "Built: $(MOUNT_FUSE_BIN)"

test-fuse: test-fstest test-postgresql test-git-clone

test-fstest: build-mount-fuse
	@bash run-fstest.sh $(MOUNT_FUSE_BIN) $(FSTEST_DIR)

test-postgresql: build-mount-fuse
	@bash run-postgresql.sh $(MOUNT_FUSE_BIN) "$(PG_INITDB)" "$(PG_CTL)" "$(PSQL)"

test-git-clone: build-mount-fuse
	@bash run-git-clone.sh $(MOUNT_FUSE_BIN)

bench:
	@bash run-benchmarks.sh

clean:
	rm -f $(MOUNT_FUSE_BIN)
