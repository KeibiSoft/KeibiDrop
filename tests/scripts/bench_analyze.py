#!/usr/bin/env python3
# ABOUTME: Analyzes KeibiDrop benchmark data and produces summary tables with speedup ratios

import sys
import os
from pathlib import Path

def parse_tsv(file_path):
    """Parse TSV file and return list of dicts."""
    if not os.path.exists(file_path):
        return []

    try:
        with open(file_path, 'r') as f:
            lines = [line.strip() for line in f.readlines()]
    except Exception:
        return []

    if not lines:
        return []

    header = lines[0].split('\t')
    rows = []

    for line in lines[1:]:
        if not line:
            continue
        parts = line.split('\t')
        if len(parts) != len(header):
            continue
        try:
            row = {
                'timestamp': parts[0],
                'treatment': parts[1],
                'mode': parts[2],
                'size': parts[3],
                'rep': int(parts[4]),
                'mbps': float(parts[5]),
                'wall_sec': float(parts[6]),
            }
            rows.append(row)
        except (ValueError, IndexError):
            continue

    return rows

def compute_stats(rows):
    """
    Organize data by (treatment, size, mode) and compute mean/stddev for reps 2-4.
    Returns dict: {(treatment, size, mode): {'mean': float, 'stddev': float, 'values': [...]}}
    """
    # Organize by cell
    cells = {}
    for row in rows:
        key = (row['treatment'], row['size'], row['mode'])
        if key not in cells:
            cells[key] = []
        cells[key].append(row)

    stats = {}
    for key, cell_rows in cells.items():
        # Filter to reps 2-4 (exclude rep 1 which is warmup)
        filtered = [r for r in cell_rows if r['rep'] in [2, 3, 4]]
        if not filtered:
            continue

        values = [r['mbps'] for r in filtered]
        mean = sum(values) / len(values)

        # Compute sample standard deviation
        if len(values) > 1:
            variance = sum((x - mean) ** 2 for x in values) / (len(values) - 1)
            stddev = variance ** 0.5
        else:
            stddev = 0.0

        stats[key] = {
            'mean': mean,
            'stddev': stddev,
            'values': values,
        }

    return stats

def format_throughput(mean, stddev):
    """Format throughput as 'mean ±stddev' or single value if stddev is 0."""
    if stddev < 0.01:
        return f"{mean:.1f}"
    return f"{mean:.1f} ±{stddev:.1f}"

def build_table_1(stats, sizes, modes, treatments):
    """Build throughput table."""
    lines = []

    # Header
    header = "Size   | Mode    | T0 (cp) | T1 (current) | T2 (reverted) | T3 (adaptive)"
    lines.append(header)
    lines.append("-" * len(header))

    # Rows
    for size in sizes:
        for mode in modes:
            row_label = f"{size:6s} | {mode:7s}"
            cells_values = []

            for treatment in treatments:
                key = (treatment, size, mode)
                if key in stats:
                    mean = stats[key]['mean']
                    stddev = stats[key]['stddev']
                    formatted = format_throughput(mean, stddev)
                else:
                    formatted = "-"
                cells_values.append(formatted)

            # Align columns: T0=10, T1=13, T2=14, T3=11
            row_content = f"{cells_values[0]:>10} | {cells_values[1]:>13} | {cells_values[2]:>14} | {cells_values[3]:>11}"
            lines.append(f"{row_label} | {row_content}")

    return lines

def build_table_2(stats, sizes, modes):
    """Build speedup ratio table (T2/T1 and T3/T1)."""
    lines = []

    # Header
    header = "Size   | Mode    | T2/T1 | T3/T1"
    lines.append(header)
    lines.append("-" * len(header))

    # Rows
    for size in sizes:
        for mode in modes:
            row_label = f"{size:6s} | {mode:7s}"

            key_t1 = ('T1', size, mode)
            key_t2 = ('T2', size, mode)
            key_t3 = ('T3', size, mode)

            if key_t1 in stats and key_t2 in stats:
                ratio_t2_t1 = stats[key_t2]['mean'] / stats[key_t1]['mean']
                t2_t1_str = f"{ratio_t2_t1:.2f}x"
            else:
                t2_t1_str = "-"

            if key_t1 in stats and key_t3 in stats:
                ratio_t3_t1 = stats[key_t3]['mean'] / stats[key_t1]['mean']
                t3_t1_str = f"{ratio_t3_t1:.2f}x"
            else:
                t3_t1_str = "-"

            row_content = f"{t2_t1_str:>6} | {t3_t1_str:>6}"
            lines.append(f"{row_label} | {row_content}")

    return lines

def compute_conclusion(stats, sizes, modes):
    """
    Determine conclusion based on decision matrix.
    """
    # Collect all T2/T1 and T3/T1 ratios (only for cells with both)
    t2_t1_ratios = []
    t3_t1_ratios = []

    for size in sizes:
        for mode in modes:
            key_t1 = ('T1', size, mode)
            key_t2 = ('T2', size, mode)
            key_t3 = ('T3', size, mode)

            if key_t1 in stats and key_t2 in stats:
                ratio = stats[key_t2]['mean'] / stats[key_t1]['mean']
                t2_t1_ratios.append(ratio)

            if key_t1 in stats and key_t3 in stats:
                ratio = stats[key_t3]['mean'] / stats[key_t1]['mean']
                t3_t1_ratios.append(ratio)

    if not t2_t1_ratios:
        return "CONCLUSION: Mixed results — review table manually"

    # Check if T2 >> T1 everywhere (ratio > 1.1)
    t2_significantly_better = all(r > 1.1 for r in t2_t1_ratios)

    # Check if T2 ~= T1 everywhere (ratio < 1.1)
    t2_similar = all(r < 1.1 for r in t2_t1_ratios)

    if not t2_significantly_better and not t2_similar:
        return "CONCLUSION: Mixed results — review table manually"

    if t2_similar:
        return "CONCLUSION: Regression NOT from pull strategy removal — investigate BlockSize/ChunkSize mismatch"

    # T2 >> T1 everywhere
    if t3_t1_ratios:
        # Check if T3 >= T2 everywhere
        t3_at_least_t2 = all(t3 >= t2 * 0.95 for t3, t2 in zip(t3_t1_ratios, t2_t1_ratios))

        # Check if T3 ~= T2 (within 5%)
        t3_similar_t2 = all(abs(t3 - t2) / t2 <= 0.05 for t3, t2 in zip(t3_t1_ratios, t2_t1_ratios))

        if t3_at_least_t2 and not t3_similar_t2:
            return "CONCLUSION: Adaptive is strictly better — ship T3"

        if t3_similar_t2:
            return "CONCLUSION: Streaming is the key fix — revert 0b8ed91"

    return "CONCLUSION: Mixed results — review table manually"

def main():
    # Determine input file
    if len(sys.argv) > 1:
        raw_tsv = sys.argv[1]
    else:
        raw_tsv = '/tmp/kd-bench-raw.tsv'

    # Parse data
    rows = parse_tsv(raw_tsv)

    if not rows:
        print("No data yet.")
        return 0

    # Compute stats
    stats = compute_stats(rows)

    if not stats:
        print("No data yet.")
        return 0

    # Define dimensions
    sizes = ['10MB', '100MB', '1GB']
    modes = ['no-FUSE', 'cp']
    treatments = ['T0', 'T1', 'T2', 'T3']

    # Build and print tables
    print("\n=== Table 1: Mean Throughput (MB/s) ===\n")
    table_1 = build_table_1(stats, sizes, modes, treatments)
    for line in table_1:
        print(line)

    print("\n=== Table 2: Speedup Ratios (T2/T1 and T3/T1) ===\n")
    table_2 = build_table_2(stats, sizes, modes)
    for line in table_2:
        print(line)

    print("\n" + compute_conclusion(stats, sizes, modes) + "\n")

    return 0

if __name__ == '__main__':
    sys.exit(main())
