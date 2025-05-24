package ui

import (
	"bytes"
	"image"
	"image/png"
	"log"
	"os"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"
)

func Launch() {
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
	// TODO: use the logo.
	// w.SetIcon(fyne.NewStaticResource("logo.png", encodeImage(img)))
	_ = encodeImage(img)
	topBar := container.NewHBox(logo, layout.NewSpacer())

	title := widget.NewLabelWithStyle("KeibiDrop", fyne.TextAlignCenter, fyne.TextStyle{Bold: true})
	title.TextStyle.Monospace = true

	startBtn := widget.NewButton("Start Session (Alice)", func() {
		// TODO: Alice session logic
	})

	joinBtn := widget.NewButton("Join Session (Bob)", func() {
		// TODO: Bob session logic
	})

	exitBtn := widget.NewButton("Exit", func() {
		a.Quit()
	})

	info := widget.NewLabel("Secure file sharing with post-quantum keys")
	info.Wrapping = fyne.TextWrapWord
	info.Alignment = fyne.TextAlignCenter

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
