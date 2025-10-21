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
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

const KEIBIDROP_RELAY_ENV = "KEIBIDROP_RELAY"
const INBOUND_PORT_ENV = "INBOUND_PORT"
const OUTBOUND_PORT_ENV = "OUTBOUND_PORT"
const TO_MOUNT_PATH_ENV = "TO_MOUNT_PATH"
const TO_SAVE_PATH_ENV = "TO_SAVE_PATH"

func Launch(logger *slog.Logger) {
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

	kd, err := common.NewKeibiDrop(ctx, logger, relayURL, inbound, outbound, toMount, toSave)
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

	// show IPv6 + relay normally
	localInfo := widget.NewLabel(fmt.Sprintf("Local IPv6: %s\nRelay: %s",
		kd.LocalIPv6IP, relayURL.String()))

	localBox := container.NewVBox(
		widget.NewLabelWithStyle("Your Fingerprint:", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		container.NewBorder(nil, nil, nil, copyBtn, localFingerprintLabel),
		localInfo,
	)

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
		err := kd.JoinRoom()
		if err != nil {
			logger.Error("Failed to join room", "error", err)
		} else {
			logger.Info("Joined room successfully")
			peerFingerprint, _ := kd.GetPeerFingerprint()
			peerInfo.SetText(fmt.Sprintf("Peer Fingerprint: %s\nPeer IPv6: %s",
				peerFingerprint, kd.PeerIPv6IP))
			peerInfo.Show()
		}
	})

	peerBox := container.NewVBox(
		widget.NewLabelWithStyle("Peer", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		peerInput,
		peerInfo,
	)

	// --- Start room (Alice) ---
	startBtn := widget.NewButton("Create Room", func() {
		err := kd.CreateRoom()
		if err != nil {
			logger.Error("Failed to create room", "error", err)
		} else {
			logger.Info("Room created successfully")
			peerFingerprint, _ := kd.GetPeerFingerprint()
			peerInfo.SetText(fmt.Sprintf("Peer Fingerprint: %s\nPeer IPv6: %s",
				peerFingerprint, kd.PeerIPv6IP))
			peerInfo.Show()
		}
	})

	exitBtn := widget.NewButton("Exit", func() {
		a.Quit()
	})

	// --- Layout ---
	content := container.NewVBox(
		topBar,
		layout.NewSpacer(),
		localBox,
		layout.NewSpacer(),
		peerBox,
		layout.NewSpacer(),
		startBtn,
		joinBtn,
		exitBtn,
		layout.NewSpacer(),
	)

	w.SetContent(container.NewPadded(content))
	w.Resize(fyne.NewSize(800, 500))
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
