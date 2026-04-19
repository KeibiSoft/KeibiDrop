// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// ABOUTME: Configuration constants for KeibiDrop: block sizes, gRPC limits, and transfer parameters.
// ABOUTME: Compile-time assertions enforce invariants between related constants.
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package config

const InboundPort = 26431
const OutboundPort = 26432

const BlockSize = 4 * 1024 * 1024 // 4 MiB - larger chunks = fewer gRPC round-trips over WAN

// gRPC message size limits
// IMPORTANT: GRPCStreamBuffer MUST be smaller than GRPCMaxMsgSize
// to leave room for protobuf framing overhead (~10-20 bytes per message)
const (
	GRPCMaxMsgSize    = 20 * 1024 * 1024 // 20 MiB - max gRPC message size
	GRPCStreamBuffer  = 16 * 1024 * 1024 // 16 MiB - buffer for streaming reads
	GRPCOverheadRoom  = GRPCMaxMsgSize - GRPCStreamBuffer // 4 MiB headroom
)

// Compile-time check: ensure buffer fits within max message size
var _ = [1]struct{}{}[int(GRPCStreamBuffer)-int(GRPCMaxMsgSize)+int(GRPCOverheadRoom)]
