// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package common

import (
	"fmt"
)

var (
	Version    = "0.3.1"
	CommitHash = "dev" // Build ldflags overwrite this value.
)

const keibiLogo = `
██╗  ██╗███████╗██╗██████╗ ██╗██████╗ ██████╗  ██████╗ ██████╗ 
██║ ██╔╝██╔════╝██║██╔══██╗██║██╔══██╗██╔══██╗██╔═══██╗██╔══██╗
█████╔╝ █████╗  ██║██████╔╝██║██║  ██║██████╔╝██║   ██║██████╔╝
██╔═██╗ ██╔══╝  ██║██╔══██╗██║██║  ██║██╔══██╗██║   ██║██╔═══╝ 
██║  ██╗███████╗██║██████╔╝██║██████╔╝██║  ██║╚██████╔╝██║     
╚═╝  ╚═╝╚══════╝╚═╝╚═════╝ ╚═╝╚═════╝ ╚═╝  ╚═╝ ╚═════╝ ╚═╝     
`

func PrintBanner() {
	fmt.Printf("%v", keibiLogo)
	fmt.Printf("\n\nVersion: %v;\nGit Commit Hash: %v \n\n", Version, CommitHash)
}
