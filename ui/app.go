package ui

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"log"
	"net/url"
	"os"

	"github.com/KeibiSoft/KeibiDrop/pkg/config"
	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	"github.com/inconshreveable/log15"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"
)

func Launch(logger log15.Logger) {
	relay := getenv("KEIBIDROP_RELAY", "https://keibidroprelay.keibisoft.com")
	relayURL, err := url.Parse(relay)
	if err != nil {
		log.Fatalf("Invalid KEIBIDROP_RELAY: %v", err)
	}

	kd, err := common.NewKeibiDrop(logger, relayURL, config.InboundPort)
	if err != nil {
		log.Fatalf("Failed to start keibidrop: %v", err)
	}

	a := app.New()
	w := a.NewWindow("KeibiDrop")

	logoFile, err := os.Open("assets/logo.png")
	if err != nil {
		log.Printf("failed to open logo file: %v", err)
		return
	}
	defer func() { _ = logoFile.Close() }()

	img, err := png.Decode(logoFile)
	if err != nil {
		log.Printf("failed to decode logo image: %v", err)
		return
	}

	logo := canvas.NewImageFromImage(img)
	logo.SetMinSize(fyne.NewSize(64, 64))
	topBar := container.NewHBox(logo, layout.NewSpacer())
	title := widget.NewLabelWithStyle("KeibiDrop", fyne.TextAlignCenter, fyne.TextStyle{Bold: true})
	title.TextStyle.Monospace = true

	fp, err := kd.ExportFingerprint()
	if err != nil {
		fp = "(error generating fingerprint)"
	}

	info := widget.NewLabel(fmt.Sprintf("IPv6: %s\nRelay: %s\nFingerprint: %s", kd.LocalIPv6IP, relayURL.String(), fp))
	info.Wrapping = fyne.TextWrapWord
	info.Alignment = fyne.TextAlignCenter

	startBtn := widget.NewButton("Start Session (Alice)", func() {
		err := kd.CreateRoom()
		if err != nil {
			log.Println("Failed to create room:", err)
		} else {
			log.Println("Room created successfully.")
		}
	})

	joinBtn := widget.NewButton("Join Session (Bob)", func() {
		input := widget.NewEntry()
		input.SetPlaceHolder("Paste peer fingerprint")

		confirm := widget.NewButton("Join", func() {
			err := kd.JoinRoom(input.Text)
			if err != nil {
				log.Println("Failed to join room:", err)
			} else {
				log.Println("Joined room successfully.")
			}
		})

		joinDialog := container.NewVBox(input, confirm)
		w.SetContent(container.NewVBox(topBar, joinDialog))
	})

	exitBtn := widget.NewButton("Exit", func() {
		a.Quit()
	})

	content := container.NewVBox(
		topBar,
		layout.NewSpacer(),
		title,
		info,
		layout.NewSpacer(),
		startBtn,
		joinBtn,
		exitBtn,
		layout.NewSpacer(),
	)

	w.SetContent(container.NewCenter(content))
	w.Resize(fyne.NewSize(480, 320))
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
