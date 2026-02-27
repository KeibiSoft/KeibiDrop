// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package ui

import (
	"bytes"
	"context"
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

	kd, err := common.NewKeibiDrop(ctx, logger, isFUSE, relayURL, inbound, outbound, toMount, toSave, false, false)
	if err != nil {
		logger.Error("Failed to create new Keibi Drop client", "error", err)
		os.Exit(1)
	}
	go kd.Run()

	a := app.New()
	a.Settings().SetTheme(theme.DarkTheme())

	w := a.NewWindow("KeibiDrop")

	// --- Status Bar ---
	statusLabel := widget.NewLabel("Status: Disconnected")

	// --- Logo & Title ---
	var logo fyne.CanvasObject
	logoFile, err := os.Open("assets/logo.png")
	if err == nil {
		img, err := png.Decode(logoFile)
		if err == nil {
			logoImg := canvas.NewImageFromImage(img)
			logoImg.FillMode = canvas.ImageFillContain
			logoImg.SetMinSize(fyne.NewSize(48, 48))
			logo = logoImg
		}
		_ = logoFile.Close()
	}
	if logo == nil {
		logo = widget.NewIcon(theme.InfoIcon())
	}

	title := widget.NewLabelWithStyle("KeibiDrop",
		fyne.TextAlignLeading,
		fyne.TextStyle{Bold: true})

	topBar := container.NewHBox(logo, title, layout.NewSpacer(), statusLabel)

	// --- Local info (Connection Tab) ---
	fp, err := kd.ExportFingerprint()
	if err != nil {
		fp = "(error)"
	}

	localFingerprintLabel := widget.NewLabel(fp)
	localFingerprintLabel.Wrapping = fyne.TextWrapWord

	copyBtn := widget.NewButtonWithIcon("Copy", theme.ContentCopyIcon(), func() {
		w.Clipboard().SetContent(fp)
	})

	localInfo := widget.NewLabel(fmt.Sprintf("Local IPv6: %s\nRelay: %s",
		kd.LocalIPv6IP, relayURL.String()))

	localSection := container.NewVBox(
		widget.NewLabelWithStyle("Your Fingerprint:", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		localFingerprintLabel,
		container.NewHBox(copyBtn),
		localInfo,
	)

	// --- Peer info (Connection Tab) ---
	peerInput := widget.NewEntry()
	peerInput.SetPlaceHolder("Enter peer fingerprint")

	peerInfo := widget.NewLabel("Peer Fingerprint: (not set)")
	peerInfo.Wrapping = fyne.TextWrapWord

	peerInput.OnChanged = func(text string) {
		if text != "" {
			peerInfo.SetText(fmt.Sprintf("Peer Fingerprint: %s", text))
			_ = kd.AddPeerFingerprint(peerInput.Text)
		} else {
			peerInfo.SetText("Peer Fingerprint: (not set)")
		}
	}

	peerSection := container.NewVBox(
		widget.NewLabelWithStyle("Peer Connection:", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		peerInput,
		peerInfo,
	)

	// --- File operations (Files Tab) ---
	addFileBtn := widget.NewButtonWithIcon("Add File to Share", theme.ContentAddIcon(), func() {
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

	listFilesBtn := widget.NewButtonWithIcon("Refresh/List Shared Files", theme.ListIcon(), func() {
		remote, local := kd.ListFiles()
		msg := "Local Shared:\n"
		for _, f := range local {
			msg += " • " + f + "\n"
		}
		msg += "\nRemote Shared:\n"
		for _, f := range remote {
			msg += " • " + f + "\n"
		}
		dialog.ShowInformation("Shared Files", msg, w)
	})

	pullFileEntry := widget.NewEntry()
	pullFileEntry.SetPlaceHolder("Remote filename to pull")

	pullFileBtn := widget.NewButtonWithIcon("Pull from Peer", theme.DownloadIcon(), func() {
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

	fileActions := container.NewVBox(
		widget.NewLabelWithStyle("Share Local Files:", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		addFileBtn,
		listFilesBtn,
		widget.NewSeparator(),
		widget.NewLabelWithStyle("Download Remote Files:", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		pullFileEntry,
		pullFileBtn,
	)
	fileActions.Hide()

	filesPlaceholder := widget.NewLabel("No active session. Go to 'Connection' tab to start.")
	filesPlaceholder.Alignment = fyne.TextAlignCenter

	// --- Tabs setup ---
	connTab := container.NewVScroll(container.NewVBox(
		localSection,
		widget.NewSeparator(),
		peerSection,
	))

	filesTab := container.NewVScroll(container.NewVBox(
		filesPlaceholder,
		fileActions,
	))

	tabs := container.NewAppTabs(
		container.NewTabItemWithIcon("Connection", theme.SettingsIcon(), connTab),
		container.NewTabItemWithIcon("Files", theme.FolderIcon(), filesTab),
	)

	// --- Action Handlers ---
	onConnected := func() {
		peerFingerprint, _ := kd.GetPeerFingerprint()
		peerIP := kd.PeerIPv6IP
		fyne.Do(func() {
			statusLabel.SetText("Status: Connected")
			peerInfo.SetText(fmt.Sprintf("Peer Fingerprint: %s\nPeer IPv6: %s", peerFingerprint, peerIP))
			filesPlaceholder.Hide()
			fileActions.Show()
			tabs.SelectIndex(1)
			dialog.ShowInformation("Connected", "Secure session established!", w)
		})
	}

	joinBtn := widget.NewButtonWithIcon("Join Room", theme.LoginIcon(), func() {
		if peerInput.Text == "" {
			dialog.ShowInformation("Input Error", "Please enter peer fingerprint.", w)
			return
		}
		statusLabel.SetText("Status: Joining...")
		go func() {
			err := kd.JoinRoom()
			if err != nil {
				fyne.Do(func() {
					statusLabel.SetText("Status: Error")
					dialog.ShowError(err, w)
				})
				return
			}
			onConnected()
		}()
	})

	startBtn := widget.NewButtonWithIcon("Create Room", theme.ContentAddIcon(), func() {
		statusLabel.SetText("Status: Creating...")
		go func() {
			err := kd.CreateRoom()
			if err != nil {
				fyne.Do(func() {
					statusLabel.SetText("Status: Error")
					dialog.ShowError(err, w)
				})
				return
			}
			onConnected()
		}()
	})

	exitBtn := widget.NewButtonWithIcon("Quit", theme.LogoutIcon(), func() {
		a.Quit()
	})

	// Add buttons to connection tab content
	// connTab is a *container.Scroll, its Content is a *fyne.Container (VBox)
	if content, ok := connTab.Content.(*fyne.Container); ok {
		content.Add(widget.NewSeparator())
		content.Add(container.NewHBox(layout.NewSpacer(), startBtn, joinBtn, layout.NewSpacer()))
	}

	// --- Final Window Layout ---
	bottomBar := container.NewHBox(layout.NewSpacer(), exitBtn)
	mainLayout := container.NewBorder(topBar, bottomBar, nil, nil, tabs)

	w.SetContent(container.NewPadded(mainLayout))
	w.Resize(fyne.NewSize(800, 600))
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
