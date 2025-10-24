// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package ui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"log/slog"
	"net/url"
	"os"
	"strconv"

	"github.com/KeibiSoft/KeibiDrop/pkg/config"
	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

const KEIBIDROP_RELAY_ENV = "KEIBIDROP_RELAY"
const INBOUND_PORT_ENV = "INBOUND_PORT"
const OUTBOUND_PORT_ENV = "OUTBOUND_PORT"
const TO_MOUNT_PATH_ENV = "TO_MOUNT_PATH"
const TO_SAVE_PATH_ENV = "TO_SAVE_PATH"

func Launch(logger *slog.Logger, isFUSE bool) {
	relay := getenv(KEIBIDROP_RELAY_ENV, "https://keibidroprelay.keibisoft.com")
	relayURL, err := url.Parse(relay)
	if err != nil {
		logger.Error("Failed to parse relay endpoint", "error", err)
		os.Exit(1)
	}

	inboundStr := os.Getenv(INBOUND_PORT_ENV)
	outboundStr := os.Getenv(OUTBOUND_PORT_ENV)
	inbound := config.InboundPort
	if inboundStr != "" {
		inPort, err := strconv.Atoi(inboundStr)
		if err != nil {
			logger.Error("Invalid inbound port", "provided", inboundStr)
			os.Exit(1)
		}
		inbound = inPort
	}
	outbound := config.OutboundPort
	if outboundStr != "" {
		outPort, err := strconv.Atoi(outboundStr)
		if err != nil {
			logger.Error("Invalid outbound port", "provided", outboundStr)
			os.Exit(1)
		}
		outbound = outPort
	}

	toMount := os.Getenv(TO_MOUNT_PATH_ENV)
	toSave := os.Getenv(TO_SAVE_PATH_ENV)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	kd, err := common.NewKeibiDrop(ctx, logger, isFUSE, relayURL, inbound, outbound, toMount, toSave)
	if err != nil {
		logger.Error("Failed to create new Keibi Drop client", "error", err)
		os.Exit(1)
	}
	go kd.Run()

	a := app.New()

	a.Settings().SetTheme(theme.DarkTheme())

	w := a.NewWindow("KeibiDrop")

	// Logo
	logoFile, _ := os.Open("assets/logo.png")
	img, _ := png.Decode(logoFile)
	logo := canvas.NewImageFromImage(img)
	logo.FillMode = canvas.ImageFillContain // preserve aspect ratio
	logo.SetMinSize(fyne.NewSize(130, 125)) // ~ your logo size
	// logo.SetMaxSize(fyne.NewSize(130, 125))

	title := widget.NewLabelWithStyle("KeibiDrop",
		fyne.TextAlignLeading,
		fyne.TextStyle{Bold: true})

	// place logo + title in the top bar
	topBar := container.NewHBox(logo, layout.NewSpacer(), title)

	// --- Local info ---
	fp, err := kd.ExportFingerprint()
	if err != nil {
		fp = "(error generating fingerprint)"
	}

	localFingerprintLabel := widget.NewLabel(fp)
	localFingerprintLabel.Wrapping = fyne.TextWrapWord

	copyBtn := widget.NewButton("Copy", func() {
		w.Clipboard().SetContent(fp)
	})

	// File operations

	// --- File operations ---
	addFileBtn := widget.NewButton("Add File", func() {
		fd := dialog.NewFileOpen(
			func(uc fyne.URIReadCloser, err error) {
				if err != nil || uc == nil {
					return
				}
				path := uc.URI().Path()
				if err := kd.AddFile(path); err != nil {
					dialog.ShowError(err, w)
				} else {
					dialog.ShowInformation("Added File", path, w)
				}
			}, w)
		fd.Show()
	})

	listFilesBtn := widget.NewButton("List Files", func() {
		remote, local := kd.ListFiles()
		msg := "Local Files:\n"
		for _, f := range local {
			msg += " • " + f + "\n"
		}
		msg += "\nRemote Files:\n"
		for _, f := range remote {
			msg += " • " + f + "\n"
		}
		dialog.ShowInformation("Shared Files", msg, w)
	})

	pullFileEntry := widget.NewEntry()
	pullFileEntry.SetPlaceHolder("Remote path to pull")

	pullFileBtn := widget.NewButton("Pull File", func() {
		remote := pullFileEntry.Text
		if remote == "" {
			return
		}
		local := toSave + "/" + remote
		if err := kd.PullFile(remote, local); err != nil {
			dialog.ShowError(err, w)
		} else {
			dialog.ShowInformation("Pulled File", fmt.Sprintf("%s -> %s", remote, local), w)
		}
	})

	// show IPv6 + relay normally
	localInfo := widget.NewLabel(fmt.Sprintf("Local IPv6: %s\nRelay: %s",
		kd.LocalIPv6IP, relayURL.String()))

	localBox := container.NewVBox(
		widget.NewLabelWithStyle("Your Fingerprint:", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		container.NewBorder(nil, nil, nil, copyBtn, localFingerprintLabel),
		localInfo,
	)

	// Wrap in a VBox and hide initially
	fileBox := container.NewVBox(localBox, addFileBtn, listFilesBtn,
		pullFileEntry,
		pullFileBtn)
	fileBox.Hide()

	// --- Peer input & info ---
	peerInput := widget.NewEntry()
	peerInput.SetPlaceHolder("Enter peer fingerprint")

	peerInfo := widget.NewLabel("Peer Fingerprint: (not set)")
	peerInfo.Wrapping = fyne.TextWrapWord

	peerInput.OnChanged = func(text string) {
		if text != "" {
			peerInfo.SetText(fmt.Sprintf("Peer Fingerprint: %s", text))
			err = kd.AddPeerFingerprint(peerInput.Text)
			if err != nil {
				logger.Error("Failed to add fingerprint", "error", err)
			}
		} else {
			peerInfo.SetText("Peer Fingerprint: (not set)")
		}
	}

	joinBtn := widget.NewButton("Join Room", func() {
		if peerInput.Text == "" {
			return
		}

		if kd.OpInProgress.Add(1) != 1 {
			kd.OpInProgress.Add(-1)
			dialog.ShowInformation("Busy", "Create/Join Room already in progress...", w)
			return
		}

		go func() {
			defer kd.OpInProgress.Add(-1)

			err := kd.JoinRoom()
			if err != nil {
				fyne.Do(func() {
					if errors.Is(err, common.ErrRateLimitHit) {
						dialog.ShowInformation("Rate Limit", "Free public relay allows ~3 joins per 5 min", w)
					} else if errors.Is(err, common.ErrServerAtCapacity) {
						dialog.ShowInformation("Relay Full", "Free public relay at capacity. Retry in 5 minutes.", w)
					} else {
						dialog.ShowError(err, w)
					}
				})
				return
			}

			peerFingerprint, _ := kd.GetPeerFingerprint()
			peerIP := kd.PeerIPv6IP

			fyne.Do(func() {
				peerInfo.SetText(fmt.Sprintf("Peer Fingerprint: %s\nPeer IPv6: %s", peerFingerprint, peerIP))
				peerInfo.Show()
				dialog.ShowInformation("Joined Room", "Successfully joined room.", w)
				fileBox.Show()
			})
		}()
	})

	peerBox := container.NewVBox(
		widget.NewLabelWithStyle("Peer", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		peerInput,
		peerInfo,
	)

	// --- Start room (Alice) ---
	startBtn := widget.NewButton("Create Room", func() {
		if kd.OpInProgress.Add(1) != 1 {
			kd.OpInProgress.Add(-1)
			dialog.ShowInformation("Busy", "Create/Join Room already in progress...", w)
			return
		}

		go func() {
			defer kd.OpInProgress.Add(-1)

			err := kd.CreateRoom()
			if err != nil {
				fyne.Do(func() {
					dialog.ShowError(err, w)
				})
				return
			}

			peerFingerprint, _ := kd.GetPeerFingerprint()
			peerIP := kd.PeerIPv6IP

			// Update UI safely
			fyne.Do(func() {
				peerInfo.SetText(fmt.Sprintf("Peer Fingerprint: %s\nPeer IPv6: %s", peerFingerprint, peerIP))
				peerInfo.Show()
				dialog.ShowInformation("Room Created", "Room created and peer connected successfully.", w)
				fileBox.Show()
			})
		}()
	})

	exitBtn := widget.NewButton("Exit", func() {
		a.Quit()
	})

	// --- Layout ---
	content := container.NewVBox(
		topBar,
		layout.NewSpacer(),
		localBox,
		fileBox,
		layout.NewSpacer(),
		peerBox,
		layout.NewSpacer(),
		startBtn,
		joinBtn,
		exitBtn,
		layout.NewSpacer(),
	)

	w.SetContent(container.NewPadded(content))
	w.Resize(fyne.NewSize(900, 600))
	w.ShowAndRun()
}

func encodeImage(img image.Image) []byte {
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func getenv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}
