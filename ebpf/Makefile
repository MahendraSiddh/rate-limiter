# =============================================================================
# Makefile — eBPF TC IP-blocker build system
#
# Targets:
#   make all        — build everything (C → .o, Go bindings, Go binary)
#   make bpf        — compile tc_blocker.c → tc_blocker.o with clang
#   make generate   — run bpf2go to generate Go skeleton wrappers
#   make loader     — build the Go HTTP loader binary
#   make clean      — remove all build artifacts
#   make load       — compile + attach the filter (root required)
#   make unload     — remove TC qdisc/filter from IFACE
#   make docker     — build a Docker image with CAP_NET_ADMIN, CAP_BPF
#   make fmt        — clang-format the C source
#
# Variables (override on command line):
#   IFACE=eth0        network interface to attach to
#   BPF_SRC=...       eBPF C source file
#   BPF_OBJ=...       output ELF object
#   LOADER_BIN=...    output Go binary name
# =============================================================================

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
IFACE       ?= eth0
BPF_SRC     := tc_blocker.c
BPF_OBJ     := tc_blocker.o
LOADER_DIR  := loader
LOADER_BIN  := tc-loader
SETUP_SCRIPT:= scripts/setup_tc.sh
IMAGE_NAME  := ebpf-tc-blocker

# Clang target triple for BPF (architecture-independent bytecode).
# -O2        : required — eBPF verifier rejects unoptimised loads on many helpers
# -target bpf: emit BPF bytecode (not host machine code)
# -g         : include BTF debug info (needed for bpftool, CO-RE)
# -mcpu=v3   : enable BPF ISA v3 extensions (JIT-friendly on 5.15+)
CLANG       ?= clang
CLANG_FLAGS := \
    -O2 \
    -g \
    -target bpf \
    -mcpu=v3 \
    -D__TARGET_ARCH_x86_64 \
    -Wall \
    -Wno-unused-value \
    -Wno-pointer-sign \
    -Wno-compare-distinct-pointer-types

# Include paths: pick up system libbpf headers.
# On Ubuntu:  apt install libbpf-dev
# On Fedora:  dnf install libbpf-devel
INCLUDES := \
    -I/usr/include \
    -I/usr/include/x86_64-linux-gnu

# bpf2go: generates Go type wrappers + embed for the BPF skeleton.
# Install: go install github.com/cilium/ebpf/cmd/bpf2go@latest
BPF2GO ?= bpf2go

# ---------------------------------------------------------------------------
# Phony targets
# ---------------------------------------------------------------------------
.PHONY: all bpf generate loader clean load unload docker fmt help

# Default target
all: bpf loader

# ---------------------------------------------------------------------------
# bpf — Compile C → BPF ELF object
#
# clang emits a standard ELF file with BPF bytecode in .text (and special
# sections: .maps, license). The kernel's BPF loader parses the ELF directly.
# ---------------------------------------------------------------------------
bpf: $(BPF_OBJ)

$(BPF_OBJ): $(BPF_SRC)
	@echo "  CLANG   $< → $@"
	$(CLANG) $(CLANG_FLAGS) $(INCLUDES) -c $< -o $@
	@echo "  OK      $(BPF_OBJ) compiled"
	@# Dump the program section names so we can verify SEC("tc") is present.
	@llvm-objdump --section-headers $(BPF_OBJ) 2>/dev/null | grep -E "tc|license|maps" || true

# ---------------------------------------------------------------------------
# generate — Run bpf2go to produce Go skeleton bindings
#
# bpf2go reads tc_blocker.o (or re-compiles from source) and emits:
#   loader/bpf_bpfel.go  — ELF loader + typed struct accessors for x86-64
#   loader/bpf_bpfeb.go  — big-endian variant (for completeness)
#
# The generated files let Go code do:
#   objs, _ := loadBpfObjects(nil)
#   objs.TcIngressBlocker   // *ebpf.Program
#   objs.BlockedIps          // *ebpf.Map
#
# NOTE: bpf2go requires the BPF object (or source) to be readable; run
#       `make bpf` first or let bpf2go compile it (it calls clang internally
#       when passed the -cc flag).
# ---------------------------------------------------------------------------
generate: $(BPF_OBJ)
	@echo "  BPF2GO  generating Go bindings → $(LOADER_DIR)/bpf_bpf*.go"
	cd $(LOADER_DIR) && \
	    $(BPF2GO) \
	        -cc $(CLANG) \
	        -cflags "$(CLANG_FLAGS) $(INCLUDES)" \
	        -target amd64 \
	        -output-dir . \
	        Bpf \
	        ../$(BPF_SRC)
	@echo "  OK      Go bindings generated in $(LOADER_DIR)/"

# ---------------------------------------------------------------------------
# loader — Build the Go HTTP API binary
#
# CGO_ENABLED=0  : static binary — no glibc dependency at runtime.
# -ldflags        : strip debug symbols for a smaller production binary.
#
# The binary needs CAP_NET_ADMIN (tc manipulation) and CAP_BPF (map/prog
# loading). Grant them with:
#   sudo setcap 'cap_net_admin,cap_bpf+ep' ./tc-loader
# or simply run as root inside a container with the capabilities listed
# in the Dockerfile below.
# ---------------------------------------------------------------------------
loader: generate
	@echo "  GO      building $(LOADER_BIN)"
	cd $(LOADER_DIR) && \
	    CGO_ENABLED=0 \
	    GOOS=linux \
	    go build \
	        -ldflags="-s -w" \
	        -o ../$(LOADER_BIN) \
	        .
	@echo "  OK      $(LOADER_BIN) ready"

# ---------------------------------------------------------------------------
# clean — Remove all generated artifacts
# ---------------------------------------------------------------------------
clean:
	@echo "  CLEAN"
	rm -f $(BPF_OBJ) $(LOADER_BIN)
	rm -f $(LOADER_DIR)/bpf_bpfel.go \
	      $(LOADER_DIR)/bpf_bpfeb.go \
	      $(LOADER_DIR)/bpf_bpfel.o \
	      $(LOADER_DIR)/bpf_bpfeb.o
	@echo "  OK"

# ---------------------------------------------------------------------------
# load — Compile and attach the filter (requires root / CAP_NET_ADMIN)
# ---------------------------------------------------------------------------
load: bpf
	@echo "  ATTACH  $(IFACE)"
	sudo bash $(SETUP_SCRIPT) $(IFACE) $(BPF_OBJ)

# ---------------------------------------------------------------------------
# unload — Detach TC filter and remove clsact qdisc
# ---------------------------------------------------------------------------
unload:
	@echo "  DETACH  $(IFACE)"
	sudo bash $(SETUP_SCRIPT) --teardown $(IFACE)

# ---------------------------------------------------------------------------
# docker — Build a Docker image that runs the loader with required capabilities
#
# Linux capabilities required:
#   CAP_NET_ADMIN  — create/modify TC qdiscs and filters
#   CAP_BPF        — load BPF programs and create pinned maps
#   CAP_SYS_ADMIN  — (fallback on kernels < 5.8 that don't have CAP_BPF)
#
# The container also needs:
#   --pid=host      — access to host network namespace
#   --network=host  — attach to host interfaces
#   --privileged    — OR the specific cap_add list below
# ---------------------------------------------------------------------------
docker:
	@echo "  DOCKER  building $(IMAGE_NAME)"
	docker build \
	    --build-arg IFACE=$(IFACE) \
	    -t $(IMAGE_NAME):latest \
	    -f - . <<'DOCKERFILE'
FROM golang:1.22-bullseye AS builder
RUN apt-get update && apt-get install -y \
        clang llvm libbpf-dev \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /build
COPY . .
RUN make bpf loader

FROM debian:bullseye-slim
RUN apt-get update && apt-get install -y iproute2 && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=builder /build/tc_blocker.o .
COPY --from=builder /build/tc-loader .
COPY scripts/setup_tc.sh scripts/

# Run the loader; TC attachment happens at startup.
# Required docker run flags:
#   --cap-add=NET_ADMIN --cap-add=BPF
#   --network=host
#   --volume=/sys/fs/bpf:/sys/fs/bpf
ENV IFACE=eth0
CMD ["./tc-loader"]
DOCKERFILE
	@echo "  OK      docker image $(IMAGE_NAME):latest"
	@echo ""
	@echo "  Run with:"
	@echo "    docker run --rm \\"
	@echo "      --cap-add=NET_ADMIN --cap-add=BPF \\"
	@echo "      --network=host \\"
	@echo "      -v /sys/fs/bpf:/sys/fs/bpf \\"
	@echo "      -e IFACE=$(IFACE) \\"
	@echo "      $(IMAGE_NAME):latest"

# ---------------------------------------------------------------------------
# fmt — Format C source with clang-format
# ---------------------------------------------------------------------------
fmt:
	@echo "  FMT     $(BPF_SRC)"
	clang-format -i --style="{BasedOnStyle: LLVM, IndentWidth: 4, ColumnLimit: 100}" $(BPF_SRC)
	@echo "  OK"

# ---------------------------------------------------------------------------
# help
# ---------------------------------------------------------------------------
help:
	@echo ""
	@echo "  eBPF TC IP-blocker — build targets:"
	@echo ""
	@echo "    make all          Build everything (default)"
	@echo "    make bpf          Compile tc_blocker.c → tc_blocker.o"
	@echo "    make generate     Run bpf2go → Go skeleton files"
	@echo "    make loader       Build Go HTTP API binary"
	@echo "    make clean        Remove build artifacts"
	@echo "    make load         Compile + attach TC filter  [sudo]"
	@echo "    make unload       Detach TC filter            [sudo]"
	@echo "    make docker       Build Docker image"
	@echo "    make fmt          clang-format the C source"
	@echo ""
	@echo "  Variables:"
	@echo "    IFACE=$(IFACE)    (default: eth0)"
	@echo ""
